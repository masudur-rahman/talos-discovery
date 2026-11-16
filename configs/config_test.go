package configs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	if err := DefaultConfiguration().Validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
}

func TestLoadPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	raw := "server:\n  addr: \":4000\"\nsnapshot:\n  enabled: false\n  interval: 2m\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TALOS_DISCOVERY_ADDR", ":5000")
	t.Setenv("TALOS_DISCOVERY_LANDING_ADDR", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":5000" {
		t.Errorf("addr = %q, want env to win", cfg.Server.Addr)
	}
	if cfg.Server.LandingAddr != "" {
		t.Errorf("landingAddr = %q, want empty env to turn it off", cfg.Server.LandingAddr)
	}
	if cfg.Server.MetricsAddr != ":2122" {
		t.Errorf("metricsAddr = %q, want the default kept", cfg.Server.MetricsAddr)
	}
	if cfg.Snapshot.Enabled || cfg.Snapshot.Interval != 2*time.Minute {
		t.Errorf("snapshot = %+v, want the file's values", cfg.Snapshot)
	}
	if cfg.RateLimit.Burst != 60 {
		t.Errorf("burst = %d, want the default kept", cfg.RateLimit.Burst)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := Load(""); err != nil {
		t.Errorf("missing default file should be fine: %v", err)
	}
	if _, err := Load("nope.yaml"); err == nil {
		t.Error("missing named file should be an error")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Configuration){
		"half tls":      func(c *Configuration) { c.TLS.CertFile = "tls.crt" },
		"no addr":       func(c *Configuration) { c.Server.Addr = "" },
		"bad level":     func(c *Configuration) { c.Log.Level = "loud" },
		"bad format":    func(c *Configuration) { c.Log.Format = "xml" },
		"zero burst":    func(c *Configuration) { c.RateLimit.Burst = 0 },
		"snapshot path": func(c *Configuration) { c.Snapshot.Enabled = true; c.Snapshot.Path = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfiguration()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate accepted it")
			}
		})
	}
}
