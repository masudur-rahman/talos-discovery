// Command talos-discovery is a self-hosted discovery service for Talos Linux
// clusters, speaking the same Cluster gRPC API as discovery.talos.dev.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/masudur-rahman/talos-discovery/configs"
	"github.com/masudur-rahman/talos-discovery/mux"
	"github.com/masudur-rahman/talos-discovery/server"
	"github.com/masudur-rahman/talos-discovery/state"
	"github.com/masudur-rahman/talos-discovery/web"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/siderolabs/discovery-api/api/v1alpha1/server/pb"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// version is stamped by the build: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configFile := flag.String("config", "", "config file (default is "+configs.DefaultConfigFile+")")
	healthcheck := flag.Bool("healthcheck", false, "probe the running server's /healthz and exit 0 when it answers")
	decodeSnapshot := flag.String("decode-snapshot", "", "print the snapshot file at this path as JSON and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println(version)
		return nil
	case *decodeSnapshot != "":
		return printSnapshot(*decodeSnapshot)
	}

	cfg, err := configs.Load(*configFile)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	setupLogger(cfg.Log)

	// The image is distroless: no shell, no curl. A container healthcheck
	// therefore runs the binary itself.
	if *healthcheck {
		return probe(cfg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	st := state.New()
	metrics, err := server.NewMetrics(registry, st)
	if err != nil {
		return fmt.Errorf("register metrics: %w", err)
	}
	connMetrics, err := mux.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("register connection metrics: %w", err)
	}

	if cfg.Snapshot.Enabled {
		loadSnapshot(st, cfg.Snapshot.Path, metrics)
	} else {
		slog.Info("snapshots are disabled")
	}

	peer := server.PeerResolver{
		TrustXRealIP:       cfg.Proxy.TrustXRealIP,
		TrustXForwardedFor: cfg.Proxy.TrustXForwardedFor,
	}
	grpcOpts := server.GRPCOptions{Metrics: metrics, Peer: peer}
	if cfg.RateLimit.Enabled {
		limiter := server.NewIPRateLimiter(cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst, cfg.RateLimit.MaxWait)
		go limiter.Run(ctx)
		grpcOpts.Limiter = limiter
	}

	var tlsConfig *tls.Config
	if cfg.TLS.Enabled() {
		reloader, err := server.NewCertReloader(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return err
		}
		tlsConfig = reloader.TLSConfig()
	}

	stopStreams := make(chan struct{})
	cluster := server.NewClusterServer(st, metrics, stopStreams, server.Options{
		RedirectEndpoint:         cfg.Server.RedirectEndpoint,
		DisableClientIPReporting: cfg.Server.DisableClientIPReporting,
		Peer:                     peer,
	})
	grpcServer, health := server.NewGRPCServer(cluster, grpcOpts)

	lis, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Server.Addr, err)
	}
	mainMux := mux.New(lis, tlsConfig, connMetrics)
	muxCtx, stopMux := context.WithCancel(context.Background())
	defer stopMux()

	site := web.NewSite(st)
	mainHTTP := web.HTTPServer("", site.Handler())

	errCh := make(chan error, 8)
	serve := func(name string, f func() error) {
		go func() {
			if err := f(); err != nil && !errors.Is(err, http.ErrServerClosed) &&
				!errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
				errCh <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	slog.Info("listening", "addr", lis.Addr().String(), "tls", tlsConfig != nil, "version", version)
	serve("mux", func() error { return mainMux.Serve(muxCtx) })
	serve("grpc server", func() error { return grpcServer.Serve(mainMux.GRPCListener()) })
	serve("http server", func() error { return mainHTTP.Serve(mainMux.HTTPListener()) })

	var extra []*http.Server
	for _, l := range []struct {
		name, addr string
		handler    http.Handler
	}{
		{"landing", cfg.Server.LandingAddr, site.Handler()},
		{"metrics", cfg.Server.MetricsAddr, web.MetricsHandler(registry)},
		{"debug", cfg.Server.DebugAddr, web.DebugHandler()},
	} {
		if l.addr == "" {
			continue
		}
		srv := web.HTTPServer(l.addr, l.handler)
		if l.name == "debug" {
			// A CPU profile or trace runs for as long as asked; the write
			// timeout would cut it off.
			srv.WriteTimeout = 0
		}
		extra = append(extra, srv)
		slog.Info(l.name+" listening", "addr", l.addr)
		serve(l.name+" server", srv.ListenAndServe)
	}

	go st.RunGC(ctx, cfg.State.GCInterval, metrics.ObserveGC)
	if cfg.Snapshot.Enabled {
		go runSnapshots(ctx, st, cfg.Snapshot, metrics)
	}

	if !cfg.Proxy.TrustXRealIP && !cfg.Proxy.TrustXForwardedFor {
		slog.Info("proxy headers not trusted; behind a proxy, nodes will see the proxy's address as their public IP")
	}
	site.SetReady(true)

	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	site.SetReady(false)
	health.SetServingStatus(pb.Cluster_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	stopMux()
	close(stopStreams)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), configs.ShutdownTimeout)
	defer cancel()
	gracefulStop(shutdownCtx, grpcServer)
	_ = mainHTTP.Shutdown(shutdownCtx)
	for _, srv := range extra {
		_ = srv.Shutdown(shutdownCtx)
	}

	// The last snapshot is the one that matters most: it is what the next
	// process starts from.
	if cfg.Snapshot.Enabled {
		saveSnapshot(st, cfg.Snapshot.Path, metrics)
	}
	return runErr
}

// gracefulStop lets in-flight calls finish, but not past the deadline. Watch
// streams have already been told to end.
func gracefulStop(ctx context.Context, srv *grpc.Server) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("graceful stop timed out, closing connections")
		srv.Stop()
	}
}

func runSnapshots(ctx context.Context, st *state.State, cfg configs.SnapshotConfig, metrics *server.Metrics) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			saveSnapshot(st, cfg.Path, metrics)
		}
	}
}

func saveSnapshot(st *state.State, path string, metrics *server.Metrics) {
	start := time.Now()
	saved, err := st.SaveSnapshot(path)
	took := time.Since(start)
	metrics.ObserveStorage(server.OperationSave, saved, took, err)
	if err != nil {
		slog.Error("snapshot save failed", "path", path, "error", err)
		return
	}
	slog.Debug("snapshot saved", "path", path, "clusters", saved.Clusters, "affiliates", saved.Affiliates,
		"endpoints", saved.Endpoints, "bytes", saved.Bytes, "took", took)
}

// loadSnapshot never fails startup: a snapshot that cannot be read is not
// worth refusing to serve over, since the nodes rebuild the state within one
// TTL anyway.
func loadSnapshot(st *state.State, path string, metrics *server.Metrics) {
	start := time.Now()
	loaded, err := st.LoadSnapshot(path)
	metrics.ObserveStorage(server.OperationLoad, loaded, time.Since(start), err)
	if err != nil {
		slog.Warn("snapshot not loaded, starting empty", "path", path, "error", err)
		return
	}
	slog.Info("snapshot loaded", "path", path, "clusters", loaded.Clusters, "affiliates", loaded.Affiliates, "endpoints", loaded.Endpoints)
}

func printSnapshot(path string) error {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	out, err := state.DecodeSnapshotJSON(raw)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func setupLogger(cfg configs.LogConfig) {
	level, _ := cfg.SlogLevel()
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if strings.EqualFold(cfg.Format, "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}

// probe asks the server on this container's own address whether it is up. The
// landing port is plain HTTP; without it, /healthz on the main port works too,
// over TLS when the port has it.
func probe(cfg configs.Configuration) error {
	addr, scheme := cfg.Server.LandingAddr, "http"
	if addr == "" {
		addr = cfg.Server.Addr
		if cfg.TLS.Enabled() {
			scheme = "https"
		}
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("healthcheck: bad listen address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		// The probe dials its own loopback, where the certificate's name never
		// matches; it is checking liveness, not identity.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // loopback liveness probe
	}
	resp, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", resp.StatusCode)
	}
	return nil
}
