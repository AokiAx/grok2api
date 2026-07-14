package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/config"
)

func TestLoadAppliesFileThenEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"host":"127.0.0.1",
		"port":8787,
		"default_model":"grok-file",
		"proxy_base_url":"https://example.test/v1",
		"app_key":"file-admin",
		"frontend":{"static_path":"./file-ui"}
	}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("GROK2API_PORT", "9999")
	t.Setenv("GROK2API_DEFAULT_MODEL", "grok-env")
	t.Setenv("GROK2API_APP_KEY", "env-admin")
	t.Setenv("GROK2API_DEBUG_TRACE", "true")
	t.Setenv("GROK2API_DEBUG_TRACE_DIR", "/tmp/g2a-traces")
	t.Setenv("GROK2API_FRONTEND_STATIC_PATH", "/app/frontend/dist")

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
	if got.AppKey != "env-admin" {
		t.Fatalf("app key = %q", got.AppKey)
	}
	if !got.DebugTrace {
		t.Fatal("expected DebugTrace from env")
	}
	if got.DebugTraceDir != "/tmp/g2a-traces" {
		t.Fatalf("DebugTraceDir=%q", got.DebugTraceDir)
	}
	if got.Frontend.StaticPath != "/app/frontend/dist" {
		t.Fatalf("Frontend.StaticPath=%q", got.Frontend.StaticPath)
	}
}

func TestAdminKeyPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		config config.Config
		want   string
	}{
		{name: "panel password", config: config.Config{PanelPassword: "panel", AppKey: "app", APIKey: "api"}, want: "panel"},
		{name: "app key", config: config.Config{AppKey: "app", APIKey: "api"}, want: "app"},
		{name: "api key", config: config.Config{APIKey: "api"}, want: "api"},
		{name: "open panel", config: config.Config{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.config.AdminKey(); got != test.want {
				t.Fatalf("admin key = %q; want %q", got, test.want)
			}
		})
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
	if got.MaxAttempts != 3 || got.Strategy != "round-robin" {
		t.Fatalf("pool defaults=%#v", got)
	}
	if got.Frontend.StaticPath != "" {
		t.Fatalf("frontend static path=%q, want disabled by default", got.Frontend.StaticPath)
	}
}
