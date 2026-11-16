package mux_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/masudur-rahman/talos-discovery/mux"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type harness struct {
	addr     string
	registry *prometheus.Registry
	stop     context.CancelFunc
	done     chan struct{}
}

// start runs a gRPC health server and a plain HTTP handler behind one mux, so
// each test can talk both protocols to the same port.
func start(t *testing.T, tlsConfig *tls.Config) *harness {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	metrics, err := mux.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	m := mux.New(lis, tlsConfig, metrics)

	gs := grpc.NewServer()
	healthpb.RegisterHealthServer(gs, health.NewServer())
	hs := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "site")
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Serve(ctx); close(done) }()
	go func() { _ = gs.Serve(m.GRPCListener()) }()
	go func() { _ = hs.Serve(m.HTTPListener()) }()
	t.Cleanup(func() {
		cancel()
		gs.Stop()
		_ = hs.Close()
	})
	return &harness{addr: lis.Addr().String(), registry: reg, stop: cancel, done: done}
}

func grpcCheck(t *testing.T, addr string, creds credentials.TransportCredentials) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("gRPC on the shared port: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v", resp.GetStatus())
	}
}

func httpGet(t *testing.T, client *http.Client, url string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("HTTP on the shared port: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "site" {
		t.Errorf("body = %q, want the HTTP handler", body)
	}
	if resp.ProtoMajor != 1 {
		t.Errorf("proto = %s, want HTTP/1.x", resp.Proto)
	}
}

func TestPlaintextSharesPort(t *testing.T) {
	h := start(t, nil)
	grpcCheck(t, h.addr, insecure.NewCredentials())
	httpGet(t, &http.Client{Timeout: 5 * time.Second}, "http://"+h.addr+"/")

	opened := testutil.ToFloat64(metricVec(t, h.registry, "discovery_connections_opened_total", "grpc"))
	if opened != 1 {
		t.Errorf("grpc connections opened = %v, want 1", opened)
	}
}

func TestTLSRoutesByALPN(t *testing.T) {
	h := start(t, selfSigned(t))

	// grpc-go offers only h2, as the Talos client does.
	grpcCheck(t, h.addr, credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec // test cert

	// A browser-like client offers h2 and http/1.1 and must get the site.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
		ForceAttemptHTTP2: true,
	}}
	httpGet(t, client, "https://"+h.addr+"/")
}

func TestFailedHandshakeIsRejected(t *testing.T) {
	h := start(t, selfSigned(t))

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	_, _ = io.ReadAll(conn) // the mux closes it
	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rejected(t, h.registry) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("rejected = %v, want 1", rejected(t, h.registry))
}

func TestServeReturnsOnCancel(t *testing.T) {
	h := start(t, nil)
	h.stop()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if _, err := net.DialTimeout("tcp", h.addr, 200*time.Millisecond); err == nil {
		t.Error("listener still accepting after cancel")
	}
}

func rejected(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "discovery_connections_rejected_total" {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// metricVec pulls one labelled counter back out of the registry.
func metricVec(t *testing.T, reg *prometheus.Registry, name, protocol string) prometheus.Collector {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetLabel()[0].GetValue() == protocol {
				c := prometheus.NewCounter(prometheus.CounterOpts{Name: name})
				c.Add(m.GetCounter().GetValue())
				return c
			}
		}
	}
	t.Fatalf("%s{protocol=%q} not found", name, protocol)
	return nil
}

func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
}
