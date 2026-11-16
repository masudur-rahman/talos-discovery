package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/masudur-rahman/talos-discovery/models"
	"github.com/masudur-rahman/talos-discovery/mux"
	"github.com/masudur-rahman/talos-discovery/state"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

type testServer struct {
	client   pb.ClusterClient
	state    *state.State
	stop     chan struct{}
	registry *prometheus.Registry
}

type testOptions struct {
	cluster Options
	limiter *IPRateLimiter
	tls     *tls.Config
}

// startServer runs the real gRPC stack on a loopback port, so peer addresses,
// metadata and keepalive behave as they do in production.
func startServer(t *testing.T, opts testOptions) *testServer {
	t.Helper()

	st := state.New()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry, st)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	stop := make(chan struct{})
	cluster := NewClusterServer(st, metrics, stop, opts.cluster)
	srv, _ := NewGRPCServer(cluster, GRPCOptions{
		Limiter: opts.limiter, Metrics: metrics, Peer: opts.cluster.Peer,
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Through the mux, as in production: it owns TLS, and the gRPC server
	// only ever sees decrypted connections.
	m := mux.New(lis, opts.tls, nil)
	muxCtx, stopMux := context.WithCancel(context.Background())
	go func() { _ = m.Serve(muxCtx) }()
	go func() { _ = srv.Serve(m.GRPCListener()) }()

	creds := insecure.NewCredentials()
	if opts.tls != nil {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed test cert
	}
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	ts := &testServer{client: pb.NewClusterClient(conn), state: st, stop: stop, registry: registry}
	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		_ = conn.Close()
		stopMux()
		srv.Stop()
	})
	return ts
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func update(clusterID, affiliateID string, data []byte, endpoints ...string) *pb.AffiliateUpdateRequest {
	req := &pb.AffiliateUpdateRequest{
		ClusterId:     clusterID,
		AffiliateId:   affiliateID,
		AffiliateData: data,
		Ttl:           durationpb.New(time.Minute),
	}
	for _, ep := range endpoints {
		req.AffiliateEndpoints = append(req.AffiliateEndpoints, []byte(ep))
	}
	return req
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("code = %v (%v), want %v", got, err, want)
	}
}

func TestHelloReturnsPeerIP(t *testing.T) {
	ts := startServer(t, testOptions{})

	resp, err := ts.client.Hello(ctx(t), &pb.HelloRequest{ClusterId: "c1", ClientVersion: "v1.11.0"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	addr, ok := netip.AddrFromSlice(resp.GetClientIp())
	if !ok || addr != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("client ip = %v, want 127.0.0.1", resp.GetClientIp())
	}
	if resp.Redirect != nil {
		t.Errorf("redirect = %v, want none", resp.Redirect)
	}
}

func TestHelloHeaderTrust(t *testing.T) {
	md := metadata.Pairs("x-real-ip", "203.0.113.7")

	untrusted := startServer(t, testOptions{})
	resp, err := untrusted.client.Hello(metadata.NewOutgoingContext(ctx(t), md), &pb.HelloRequest{ClusterId: "c1"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if addr, _ := netip.AddrFromSlice(resp.GetClientIp()); addr.String() != "127.0.0.1" {
		t.Errorf("untrusted header was used: %v", addr)
	}

	trusted := startServer(t, testOptions{cluster: Options{Peer: PeerResolver{TrustXRealIP: true}}})
	resp, err = trusted.client.Hello(metadata.NewOutgoingContext(ctx(t), md), &pb.HelloRequest{ClusterId: "c1"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if addr, _ := netip.AddrFromSlice(resp.GetClientIp()); addr.String() != "203.0.113.7" {
		t.Errorf("client ip = %v, want the X-Real-IP", addr)
	}
}

func TestHelloRedirect(t *testing.T) {
	ts := startServer(t, testOptions{cluster: Options{RedirectEndpoint: "new.example.com:443"}})
	resp, err := ts.client.Hello(ctx(t), &pb.HelloRequest{ClusterId: "c1"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if resp.GetRedirect().GetEndpoint() != "new.example.com:443" {
		t.Errorf("redirect = %v", resp.GetRedirect())
	}
}

func TestUpdateListDelete(t *testing.T) {
	ts := startServer(t, testOptions{})
	c := ctx(t)

	if _, err := ts.client.AffiliateUpdate(c, update("c1", "a1", []byte("d1"), "e1")); err != nil {
		t.Fatalf("AffiliateUpdate: %v", err)
	}
	if _, err := ts.client.AffiliateUpdate(c, update("c1", "a2", []byte("d2"))); err != nil {
		t.Fatalf("AffiliateUpdate: %v", err)
	}

	list, err := ts.client.List(c, &pb.ListRequest{ClusterId: "c1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := len(list.GetAffiliates()); n != 2 {
		t.Fatalf("affiliates = %d, want 2", n)
	}
	if a := list.GetAffiliates()[0]; a.GetId() != "a1" || string(a.GetData()) != "d1" || string(a.GetEndpoints()[0]) != "e1" {
		t.Errorf("a1 = %v", a)
	}

	if _, err := ts.client.AffiliateDelete(c, &pb.AffiliateDeleteRequest{ClusterId: "c1", AffiliateId: "a1"}); err != nil {
		t.Fatalf("AffiliateDelete: %v", err)
	}
	list, _ = ts.client.List(c, &pb.ListRequest{ClusterId: "c1"})
	if n := len(list.GetAffiliates()); n != 1 {
		t.Errorf("affiliates after delete = %d, want 1", n)
	}
}

func TestValidation(t *testing.T) {
	ts := startServer(t, testOptions{})
	c := ctx(t)

	long := string(make([]byte, models.ClusterIDMax+1))
	tooLongTTL := update("c1", "a1", nil)
	tooLongTTL.Ttl = durationpb.New(models.TTLMax + time.Second)

	cases := map[string]*pb.AffiliateUpdateRequest{
		"empty cluster":    update("", "a1", nil),
		"long cluster":     update(long, "a1", nil),
		"empty affiliate":  update("c1", "", nil),
		"big data":         update("c1", "a1", make([]byte, models.AffiliateDataMax+1)),
		"big endpoint":     update("c1", "a1", nil, string(make([]byte, models.AffiliateEndpointMax+1))),
		"ttl over the max": tooLongTTL,
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ts.client.AffiliateUpdate(c, req)
			wantCode(t, err, codes.InvalidArgument)
		})
	}

	_, err := ts.client.Hello(c, &pb.HelloRequest{})
	wantCode(t, err, codes.InvalidArgument)
	_, err = ts.client.List(c, &pb.ListRequest{})
	wantCode(t, err, codes.InvalidArgument)
}

func TestEndpointLimitIsResourceExhausted(t *testing.T) {
	ts := startServer(t, testOptions{})
	c := ctx(t)

	var eps []string
	for i := range models.AffiliateEndpointsMax {
		eps = append(eps, string(rune('A'+i%26))+string(rune('a'+i/26)))
	}
	if _, err := ts.client.AffiliateUpdate(c, update("c1", "a1", nil, eps...)); err != nil {
		t.Fatalf("AffiliateUpdate: %v", err)
	}
	_, err := ts.client.AffiliateUpdate(c, update("c1", "a1", nil, "one-more"))
	wantCode(t, err, codes.ResourceExhausted)
}

func TestWatch(t *testing.T) {
	ts := startServer(t, testOptions{})
	c := ctx(t)

	if _, err := ts.client.AffiliateUpdate(c, update("c1", "a1", []byte("d1"))); err != nil {
		t.Fatalf("AffiliateUpdate: %v", err)
	}

	stream, err := ts.client.Watch(c, &pb.WatchRequest{ClusterId: "c1"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}
	if len(first.GetAffiliates()) != 1 || first.GetDeleted() {
		t.Fatalf("snapshot = %v", first)
	}

	if _, err := ts.client.AffiliateUpdate(c, update("c1", "a2", []byte("d2"))); err != nil {
		t.Fatalf("AffiliateUpdate: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv update: %v", err)
	}
	if msg.GetDeleted() || msg.GetAffiliates()[0].GetId() != "a2" {
		t.Errorf("update = %v", msg)
	}

	if _, err := ts.client.AffiliateDelete(c, &pb.AffiliateDeleteRequest{ClusterId: "c1", AffiliateId: "a1"}); err != nil {
		t.Fatalf("AffiliateDelete: %v", err)
	}
	msg, err = stream.Recv()
	if err != nil {
		t.Fatalf("Recv delete: %v", err)
	}
	if !msg.GetDeleted() || msg.GetAffiliates()[0].GetId() != "a1" {
		t.Errorf("delete = %v", msg)
	}

	// Shutdown ends open streams cleanly rather than leaving them for
	// GracefulStop to wait on.
	close(ts.stop)
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after stop = %v, want a clean end of stream", err)
	}
}

func TestZeroTTLIsAcceptedAndNeverListed(t *testing.T) {
	ts := startServer(t, testOptions{})
	c := ctx(t)

	req := update("c1", "a1", []byte("d"))
	req.Ttl = nil
	if _, err := ts.client.AffiliateUpdate(c, req); err != nil {
		t.Fatalf("AffiliateUpdate without ttl: %v", err)
	}
	list, err := ts.client.List(c, &pb.ListRequest{ClusterId: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetAffiliates()) != 0 {
		t.Errorf("affiliates = %v, want the expired-on-arrival entry hidden", list.GetAffiliates())
	}
}

func TestHelloWithoutClientIP(t *testing.T) {
	ts := startServer(t, testOptions{cluster: Options{DisableClientIPReporting: true}})
	resp, err := ts.client.Hello(ctx(t), &pb.HelloRequest{ClusterId: "c1"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if len(resp.GetClientIp()) != 0 {
		t.Errorf("client ip = %v, want none", resp.GetClientIp())
	}
}

// TestMetricNames pins the series a dashboard is built on. They are the
// upstream service's names; renaming one breaks every panel that uses it.
func TestMetricNames(t *testing.T) {
	ts := startServer(t, testOptions{})
	c := ctx(t)
	_, _ = ts.client.Hello(c, &pb.HelloRequest{ClusterId: "c1", ClientVersion: "v1.11.2"})
	_, _ = ts.client.AffiliateUpdate(c, update("c1", "a1", []byte("d"), "e1"))

	families, err := ts.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}
	for _, name := range []string{
		"discovery_state_clusters", "discovery_state_affiliates", "discovery_state_endpoints", "discovery_state_subscriptions",
		"discovery_state_gc_runs_total", "discovery_state_gc_clusters_total", "discovery_state_gc_affiliates_total",
		"discovery_server_hello_requests_total",
		"discovery_storage_operations_total",
		"grpc_server_started_total", "grpc_server_handled_total", "grpc_server_msg_received_total",
		"grpc_server_msg_sent_total", "grpc_server_handling_seconds",
	} {
		if !got[name] {
			t.Errorf("metric %s is missing", name)
		}
	}
}

func TestWatchEmptyClusterSendsSnapshot(t *testing.T) {
	ts := startServer(t, testOptions{})
	stream, err := ts.client.Watch(ctx(t), &pb.WatchRequest{ClusterId: "empty"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(msg.GetAffiliates()) != 0 {
		t.Errorf("snapshot = %v, want empty", msg)
	}
}

func TestRateLimit(t *testing.T) {
	ts := startServer(t, testOptions{limiter: NewIPRateLimiter(0.001, 2, 10*time.Millisecond)})
	c := ctx(t)

	for range 2 {
		if _, err := ts.client.Hello(c, &pb.HelloRequest{ClusterId: "c1"}); err != nil {
			t.Fatalf("Hello within burst: %v", err)
		}
	}
	_, err := ts.client.Hello(c, &pb.HelloRequest{ClusterId: "c1"})
	wantCode(t, err, codes.ResourceExhausted)
}

func TestRateLimiterPrune(t *testing.T) {
	l := NewIPRateLimiter(1, 1, 0)
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Wait(context.Background(), netip.MustParseAddr("10.0.0.1"))

	now = now.Add(2 * time.Minute)
	if n := l.Prune(time.Minute); n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}
}

func TestPeerResolverForwardedFor(t *testing.T) {
	r := PeerResolver{TrustXForwardedFor: true}
	md := metadata.Pairs("x-forwarded-for", "198.51.100.1, 10.0.0.1")
	got := r.Resolve(metadata.NewIncomingContext(context.Background(), md))
	if got.String() != "198.51.100.1" {
		t.Errorf("addr = %v, want the first entry", got)
	}

	md = metadata.Pairs("x-real-ip", "::ffff:192.0.2.1")
	got = PeerResolver{TrustXRealIP: true}.Resolve(metadata.NewIncomingContext(context.Background(), md))
	if got.String() != "192.0.2.1" {
		t.Errorf("addr = %v, want the v4-mapped address unmapped", got)
	}
}

func TestVersionLabel(t *testing.T) {
	cases := map[string]string{
		"v1.11.3":        "v1.11",
		"v1.12.0-beta.1": "v1.12-pre",
		"v1.8.0":         "v1.8",
		"1.8.0":          "unknown",
		"v1.9":           "unknown",
		"":               "unknown",
		"garbage":        "unknown",
		"v1.10000.0":     "unknown",
	}
	for in, want := range cases {
		if got := versionLabel(in); got != want {
			t.Errorf("versionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTLSWithReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeCert(t, certFile, keyFile, "first")

	reloader, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	ts := startServer(t, testOptions{tls: reloader.TLSConfig()})
	if _, err := ts.client.Hello(ctx(t), &pb.HelloRequest{ClusterId: "c1"}); err != nil {
		t.Fatalf("Hello over TLS: %v", err)
	}

	// A renewal replaces the files; the next handshake after the check
	// interval serves the new certificate.
	writeCert(t, certFile, keyFile, "second")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(certFile, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	reloader.checkedAt = time.Time{}

	cert, err := reloader.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if leaf.Subject.CommonName != "second" {
		t.Errorf("serving %q, want the renewed certificate", leaf.Subject.CommonName)
	}
}

func TestCertReloaderRejectsMissingFiles(t *testing.T) {
	if _, err := NewCertReloader("/nonexistent/tls.crt", "/nonexistent/tls.key"); err == nil {
		t.Error("NewCertReloader accepted missing files")
	}
}

func writeCert(t *testing.T, certFile, keyFile, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}
