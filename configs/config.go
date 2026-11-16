// Package configs loads runtime settings.
//
// Defaults, then the config file, then the environment, in that order. Every
// field has a working default, so a plaintext server on :3000 needs no file at
// all, and a container can be driven entirely by env.
package configs

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultConfigFile is where the config is read from when no path is given.
const DefaultConfigFile = ".configs/.talos-discovery.yaml"

// ShutdownTimeout bounds how long in-flight unary calls get on SIGTERM. Watch
// streams are ended straight away; nodes reconnect on their own.
const ShutdownTimeout = 10 * time.Second

// Configuration is the whole config file.
type Configuration struct {
	Server    ServerConfig    `json:"server" yaml:"server"`
	TLS       TLSConfig       `json:"tls" yaml:"tls"`
	Proxy     ProxyConfig     `json:"proxy" yaml:"proxy"`
	RateLimit RateLimitConfig `json:"rateLimit" yaml:"rateLimit"`
	State     StateConfig     `json:"state" yaml:"state"`
	Snapshot  SnapshotConfig  `json:"snapshot" yaml:"snapshot"`
	Log       LogConfig       `json:"log" yaml:"log"`
}

// ServerConfig lays the ports out as the upstream service does, so firewall
// rules and scrape configs carry over.
type ServerConfig struct {
	// Addr is the main port. Talos nodes connect here over gRPC, and the same
	// port answers HTTP with the landing page, /inspect, /stats and /healthz.
	Addr string `json:"addr" yaml:"addr"`
	// LandingAddr serves the same pages on a port of their own. Empty turns
	// it off.
	LandingAddr string `json:"landingAddr" yaml:"landingAddr"`
	// MetricsAddr serves /metrics. Empty turns it off. It is kept off the
	// main port so metrics are never exposed through the address the nodes
	// and the public use.
	MetricsAddr string `json:"metricsAddr" yaml:"metricsAddr"`
	// DebugAddr serves pprof and expvar. Empty, the default, turns it off;
	// bind it to loopback when it is on.
	DebugAddr string `json:"debugAddr" yaml:"debugAddr"`
	// RedirectEndpoint, when set, sends every node to another server
	// (host:port) on its next Hello.
	RedirectEndpoint string `json:"redirectEndpoint" yaml:"redirectEndpoint"`
	// DisableClientIPReporting leaves the node's address out of Hello, for a
	// server that only sees nodes through NAT and would report it wrong.
	DisableClientIPReporting bool `json:"disableClientIPReporting" yaml:"disableClientIPReporting"`
}

// TLSConfig is off when both files are empty. Plaintext is the mode for
// running behind a proxy that terminates TLS, and what a Talos machine config
// with an http:// endpoint expects.
type TLSConfig struct {
	CertFile string `json:"certFile" yaml:"certFile"`
	KeyFile  string `json:"keyFile" yaml:"keyFile"`
}

// Enabled reports whether the gRPC port serves TLS.
func (c TLSConfig) Enabled() bool {
	return c.CertFile != "" || c.KeyFile != ""
}

// ProxyConfig says which headers carry the client's real address. Turn these
// on only when every request comes through a proxy that sets them: a client
// reaching the server directly could put any address it likes in them, and
// KubeSpan would trust it as that node's public IP.
type ProxyConfig struct {
	TrustXRealIP       bool `json:"trustXRealIP" yaml:"trustXRealIP"`
	TrustXForwardedFor bool `json:"trustXForwardedFor" yaml:"trustXForwardedFor"`
}

// RateLimitConfig is per client address. Behind a proxy that address is only
// the client's when the proxy headers are trusted; otherwise every node shares
// the proxy's one bucket.
type RateLimitConfig struct {
	Enabled           bool          `json:"enabled" yaml:"enabled"`
	RequestsPerSecond float64       `json:"requestsPerSecond" yaml:"requestsPerSecond"`
	Burst             int           `json:"burst" yaml:"burst"`
	MaxWait           time.Duration `json:"maxWait" yaml:"maxWait"`
}

type StateConfig struct {
	GCInterval time.Duration `json:"gcInterval" yaml:"gcInterval"`
}

// SnapshotConfig saves the state to disk so a restart does not leave every
// cluster empty until its nodes next refresh.
type SnapshotConfig struct {
	Enabled  bool          `json:"enabled" yaml:"enabled"`
	Path     string        `json:"path" yaml:"path"`
	Interval time.Duration `json:"interval" yaml:"interval"`
}

type LogConfig struct {
	// Level is debug, info, warn or error. Debug logs every request.
	Level string `json:"level" yaml:"level"`
	// Format is text or json.
	Format string `json:"format" yaml:"format"`
}

// DefaultConfiguration is what a run with no file and no environment gets.
func DefaultConfiguration() Configuration {
	return Configuration{
		Server: ServerConfig{
			Addr:        ":3000",
			LandingAddr: ":3001",
			MetricsAddr: ":2122",
		},
		RateLimit: RateLimitConfig{
			Enabled:           true,
			RequestsPerSecond: 15,
			Burst:             60,
			MaxWait:           time.Second,
		},
		State: StateConfig{GCInterval: time.Minute},
		// Snapshots are on: without one, a restart leaves every cluster
		// empty until each node's next refresh, and nodes briefly lose sight
		// of each other. The path is relative so it lands in /app/data in the
		// image and ./data in a checkout.
		Snapshot: SnapshotConfig{
			Enabled:  true,
			Path:     "data/state.binpb",
			Interval: 10 * time.Minute,
		},
		Log: LogConfig{Level: "info", Format: "text"},
	}
}

// Load reads the config file and applies environment overrides.
//
// An explicitly named file that cannot be read is an error; the default path is
// optional, because a container run is configured entirely through env.
func Load(path string) (Configuration, error) {
	cfg := DefaultConfiguration()

	explicit := path != ""
	if !explicit {
		path = env("TALOS_DISCOVERY_CONFIG", DefaultConfigFile)
		explicit = os.Getenv("TALOS_DISCOVERY_CONFIG") != ""
	}

	raw, err := os.ReadFile(filepath.Clean(path))
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
		slog.Info("loaded config file", "path", path)
	case explicit:
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	case !os.IsNotExist(err):
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg.OverrideWithEnv()
	return cfg, nil
}

// OverrideWithEnv lets the environment win over the file.
func (c *Configuration) OverrideWithEnv() {
	c.Server.Addr = env("TALOS_DISCOVERY_ADDR", c.Server.Addr)
	c.Server.LandingAddr = envAllowEmpty("TALOS_DISCOVERY_LANDING_ADDR", c.Server.LandingAddr)
	c.Server.MetricsAddr = envAllowEmpty("TALOS_DISCOVERY_METRICS_ADDR", c.Server.MetricsAddr)
	c.Server.DebugAddr = envAllowEmpty("TALOS_DISCOVERY_DEBUG_ADDR", c.Server.DebugAddr)
	c.Server.RedirectEndpoint = env("TALOS_DISCOVERY_REDIRECT_ENDPOINT", c.Server.RedirectEndpoint)
	c.Server.DisableClientIPReporting = envBool("TALOS_DISCOVERY_DISABLE_CLIENT_IP_REPORTING", c.Server.DisableClientIPReporting)

	c.TLS.CertFile = env("TALOS_DISCOVERY_TLS_CERT_FILE", c.TLS.CertFile)
	c.TLS.KeyFile = env("TALOS_DISCOVERY_TLS_KEY_FILE", c.TLS.KeyFile)

	c.Proxy.TrustXRealIP = envBool("TALOS_DISCOVERY_TRUST_X_REAL_IP", c.Proxy.TrustXRealIP)
	c.Proxy.TrustXForwardedFor = envBool("TALOS_DISCOVERY_TRUST_X_FORWARDED_FOR", c.Proxy.TrustXForwardedFor)

	c.RateLimit.Enabled = envBool("TALOS_DISCOVERY_RATE_LIMIT_ENABLED", c.RateLimit.Enabled)
	c.RateLimit.RequestsPerSecond = envFloat("TALOS_DISCOVERY_RATE_LIMIT_RPS", c.RateLimit.RequestsPerSecond)
	c.RateLimit.Burst = envInt("TALOS_DISCOVERY_RATE_LIMIT_BURST", c.RateLimit.Burst)
	c.RateLimit.MaxWait = envDuration("TALOS_DISCOVERY_RATE_LIMIT_MAX_WAIT", c.RateLimit.MaxWait)

	c.State.GCInterval = envDuration("TALOS_DISCOVERY_GC_INTERVAL", c.State.GCInterval)

	c.Snapshot.Enabled = envBool("TALOS_DISCOVERY_SNAPSHOT_ENABLED", c.Snapshot.Enabled)
	c.Snapshot.Path = env("TALOS_DISCOVERY_SNAPSHOT_PATH", c.Snapshot.Path)
	c.Snapshot.Interval = envDuration("TALOS_DISCOVERY_SNAPSHOT_INTERVAL", c.Snapshot.Interval)

	c.Log.Level = env("TALOS_DISCOVERY_LOG_LEVEL", c.Log.Level)
	c.Log.Format = env("TALOS_DISCOVERY_LOG_FORMAT", c.Log.Format)
}

// Validate reports settings that would fail at runtime.
func (c Configuration) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr must not be empty")
	}
	if c.TLS.Enabled() && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return fmt.Errorf("tls needs both certFile and keyFile, or neither for plaintext")
	}
	if c.RateLimit.Enabled && (c.RateLimit.RequestsPerSecond <= 0 || c.RateLimit.Burst <= 0) {
		return fmt.Errorf("rateLimit.requestsPerSecond and rateLimit.burst must be positive")
	}
	if c.State.GCInterval <= 0 {
		return fmt.Errorf("state.gcInterval must be positive")
	}
	if c.Snapshot.Enabled && (c.Snapshot.Path == "" || c.Snapshot.Interval <= 0) {
		return fmt.Errorf("snapshot needs a path and a positive interval when enabled")
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		return err
	}
	if f := strings.ToLower(c.Log.Format); f != "text" && f != "json" {
		return fmt.Errorf("log.format must be text or json, got %q", c.Log.Format)
	}
	return nil
}

// SlogLevel parses Level.
func (c LogConfig) SlogLevel() (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(c.Level)); err != nil {
		return 0, fmt.Errorf("log.level: %w", err)
	}
	return l, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envAllowEmpty treats a variable that is set but empty as a value, so an
// optional listener can be turned off from the environment.
func envAllowEmpty(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("ignoring invalid duration", "key", key, "value", v)
		return fallback
	}
	return d
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("ignoring invalid bool", "key", key, "value", v)
		return fallback
	}
	return b
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ignoring invalid int", "key", key, "value", v)
		return fallback
	}
	return n
}

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Warn("ignoring invalid number", "key", key, "value", v)
		return fallback
	}
	return f
}
