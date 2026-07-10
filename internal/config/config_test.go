package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/config"
)

func TestLoadAppliesFileThenEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"host":"127.0.0.1",
		"port":8787,
		"default_model":"grok-file",
		"proxy_base_url":"https://example.test/v1"
	}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("GROK2API_PORT", "9999")
	t.Setenv("GROK2API_DEFAULT_MODEL", "grok-env")

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got.Port != 9999 {
		t.Fatalf("port = %d; want 9999", got.Port)
	}
	if got.DefaultModel != "grok-env" {
		t.Fatalf("default model = %q; want grok-env", got.DefaultModel)
	}
	if got.ProxyBaseURL != "https://example.test/v1" {
		t.Fatalf("proxy URL = %q", got.ProxyBaseURL)
	}
}

func TestConfigHelpers(t *testing.T) {
	config := config.Config{Host: "0.0.0.0", Port: 8787, RequestTimeoutSec: 12}
	if config.Address() != "0.0.0.0:8787" {
		t.Fatalf("address = %q", config.Address())
	}
	if config.RequestTimeout() != 12*time.Second {
		t.Fatalf("timeout = %s", config.RequestTimeout())
	}
}

func TestLoadRejectsInvalidEnvironmentInteger(t *testing.T) {
	t.Setenv("GROK2API_PORT", "not-a-number")
	if _, err := config.Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected invalid environment integer error")
	}
}

func TestLoadRejectsMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{invalid}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected malformed config error")
	}
}

func TestLoadUsesSafeDefaultsWhenFileMissing(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if got.Host != "127.0.0.1" || got.Port != 8787 {
		t.Fatalf("listen defaults = %s:%d", got.Host, got.Port)
	}
	if got.DefaultModel == "" || got.ProxyBaseURL == "" {
		t.Fatal("upstream defaults must be populated")
	}
}
