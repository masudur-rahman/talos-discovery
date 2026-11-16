// Package e2e runs the real Talos discovery client against this server. The
// unit tests prove the server does what this code thinks the protocol is; this
// proves the protocol is what Talos actually speaks.
package e2e

import (
	"context"
	"crypto/aes"
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
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/masudur-rahman/talos-discovery/mux"
	"github.com/masudur-rahman/talos-discovery/server"
	"github.com/masudur-rahman/talos-discovery/state"
	"github.com/masudur-rahman/talos-discovery/web"

	"github.com/prometheus/client_golang/prometheus"
	clientpb "github.com/siderolabs/discovery-api/api/v1alpha1/client/pb"
	"github.com/siderolabs/discovery-client/pkg/client"
	"go.uber.org/zap/zaptest"
)

// startServer wires the stack the way main does: one port, the mux in front,
// gRPC and the site behind it.
func startServer(t *testing.T, tlsConfig *tls.Config) string {
	t.Helper()
	st := state.New()
	metrics, err := server.NewMetrics(prometheus.NewRegistry(), st)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	cluster := server.NewClusterServer(st, metrics, stop, server.Options{})
	srv, _ := server.NewGRPCServer(cluster, server.GRPCOptions{
		Metrics: metrics,
		Limiter: server.NewIPRateLimiter(15, 60, time.Second),
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := mux.New(lis, tlsConfig, nil)
	site := web.NewSite(st)
	site.SetReady(true)
	httpSrv := web.HTTPServer("", site.Handler())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = m.Serve(ctx) }()
	go func() { _ = srv.Serve(m.GRPCListener()) }()
	go func() { _ = httpSrv.Serve(m.HTTPListener()) }()
	t.Cleanup(func() {
		cancel()
		close(stop)
		srv.Stop()
		_ = httpSrv.Close()
	})
	return lis.Addr().String()
}

type node struct {
	id     string
	client *client.Client
	notify chan struct{}
}

func startNode(t *testing.T, ctx context.Context, endpoint, clusterID string, key []byte, id string, ip netip.Addr) *node {
	return startNodeTLS(t, ctx, endpoint, false, clusterID, key, id, ip)
}

func startNodeTLS(t *testing.T, ctx context.Context, endpoint string, useTLS bool, clusterID string, key []byte, id string, ip netip.Addr) *node {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.NewClient(client.Options{
		Cipher:      block,
		Endpoint:    endpoint,
		ClusterID:   clusterID,
		AffiliateID: id,
		TTL:         time.Minute,
		Insecure:    !useTLS,
		TLSConfig: func() *tls.Config {
			return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // self-signed test cert
		},
		ClientVersion: "v1.11.0",
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, _ := ip.MarshalBinary()
	err = c.SetLocalData(&client.Affiliate{
		Affiliate: &clientpb.Affiliate{
			NodeId:    id,
			Hostname:  id,
			Nodename:  id,
			Addresses: [][]byte{addr},
		},
		Endpoints: []*clientpb.Endpoint{{Ip: addr, Port: 51820}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	n := &node{id: id, client: c, notify: make(chan struct{}, 16)}
	go func() { _ = c.Run(ctx, zaptest.NewLogger(t), n.notify) }()
	return n
}

// waitFor polls until cond holds, re-checking on every client notification.
func waitFor(t *testing.T, n *node, what string, cond func([]*client.Affiliate) bool) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		if cond(n.client.GetAffiliates()) {
			return
		}
		select {
		case <-n.notify:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatalf("%s: timed out waiting for %s", n.id, what)
		}
	}
}

func hasNode(id string) func([]*client.Affiliate) bool {
	return func(affs []*client.Affiliate) bool {
		return slices.ContainsFunc(affs, func(a *client.Affiliate) bool {
			return a.Affiliate.GetNodeId() == id && len(a.Endpoints) == 1 && a.Endpoints[0].GetPort() == 51820
		})
	}
}

func TestNodesDiscoverEachOther(t *testing.T) {
	endpoint := startServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := make([]byte, 32)
	_, _ = rand.Read(key)

	a := startNode(t, ctx, endpoint, "cluster-1", key, "node-a", netip.MustParseAddr("10.5.0.2"))
	b := startNode(t, ctx, endpoint, "cluster-1", key, "node-b", netip.MustParseAddr("fd00::3"))

	waitFor(t, a, "node-b", hasNode("node-b"))
	waitFor(t, b, "node-a", hasNode("node-a"))

	// Hello told each node its address as the server sees it.
	if ip, ok := netip.AddrFromSlice(a.client.GetPublicIP()); !ok || ip.String() != "127.0.0.1" {
		t.Errorf("public ip = %v, want 127.0.0.1", a.client.GetPublicIP())
	}

	// A node leaving cleanly disappears from the others.
	b.client.DeleteLocalAffiliate()
	waitFor(t, a, "node-b to leave", func(affs []*client.Affiliate) bool { return !hasNode("node-b")(affs) })
}

func TestClustersAreIsolated(t *testing.T) {
	endpoint := startServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key1, key2 := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(key1)
	_, _ = rand.Read(key2)

	a := startNode(t, ctx, endpoint, "cluster-1", key1, "node-a", netip.MustParseAddr("10.5.0.2"))
	peer := startNode(t, ctx, endpoint, "cluster-1", key1, "node-p", netip.MustParseAddr("10.5.0.4"))
	_ = startNode(t, ctx, endpoint, "cluster-2", key2, "node-x", netip.MustParseAddr("10.6.0.2"))

	waitFor(t, a, "node-p", hasNode("node-p"))
	waitFor(t, peer, "node-a", hasNode("node-a"))
	if hasNode("node-x")(a.client.GetAffiliates()) {
		t.Error("node from another cluster leaked into cluster-1")
	}
}

// TestTLSWithSiteOnSamePort is the production shape with the server's own
// certificate: Talos nodes and a browser on one TLS port, each landing on the
// right server.
func TestTLSWithSiteOnSamePort(t *testing.T) {
	endpoint := startServer(t, selfSigned(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	a := startNodeTLS(t, ctx, endpoint, true, "cluster-tls", key, "node-a", netip.MustParseAddr("10.5.0.2"))
	b := startNodeTLS(t, ctx, endpoint, true, "cluster-tls", key, "node-b", netip.MustParseAddr("10.5.0.3"))
	waitFor(t, a, "node-b", hasNode("node-b"))
	waitFor(t, b, "node-a", hasNode("node-a"))

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		ForceAttemptHTTP2: true,
	}}
	resp, err := client.Get("https://" + endpoint + "/inspect?clusterID=cluster-tls")
	if err != nil {
		t.Fatalf("site on the gRPC port: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "2 affiliates in") {
		t.Errorf("inspect did not list both nodes:\n%s", body)
	}
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
