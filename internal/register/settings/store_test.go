package settings_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AokiAx/grok2api/internal/config"
	"github.com/AokiAx/grok2api/internal/register/settings"
)

func TestStorePersistsRegisterSettings(t *testing.T) {
	dir := t.TempDir()
	seed := config.Defaults()
	seed.EmailProvider = "cfmail"
	seed.CfmailAccounts = []config.CfmailAccount{{
		Name: "main", WorkerDomain: "mail.example.com", EmailDomain: "example.com", AdminPassword: "secret",
	}}
	store, err := settings.NewStore(dir, seed)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	got := store.Get()
	if len(got.CfmailAccounts) != 1 || got.CfmailAccounts[0].AdminPassword != "secret" {
		t.Fatalf("seeded accounts = %#v", got.CfmailAccounts)
	}

	enabled := true
	updated, err := store.Update(config.Config{
		EmailProvider: "mailtm",
		MailtmDomain:  "mail.tm",
		MaxWorkers:    3,
		TotalAccounts: 5,
		Proxy:         "http://127.0.0.1:8118",
		ProxyPool:     []string{},
		CfmailAccounts: []config.CfmailAccount{{
			Name: "main", WorkerDomain: "mail.example.com", EmailDomain: "example.com", AdminPassword: "new", Enabled: &enabled,
		}},
		CapMonsterAPIKey: "cm-key",
		TurnstileSolver:  "auto",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.EmailProvider != "mailtm" || updated.MaxWorkers != 3 || updated.CapMonsterAPIKey != "cm-key" {
		t.Fatalf("updated = %#v", updated)
	}

	reopened, err := settings.NewStore(dir, config.Defaults())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	loaded := reopened.Get()
	if loaded.EmailProvider != "mailtm" || loaded.MailtmDomain != "mail.tm" || loaded.CapMonsterAPIKey != "cm-key" {
		t.Fatalf("reloaded = %#v", loaded)
	}
	if _, err := os.Stat(filepath.Join(dir, "register_settings.json")); err != nil {
		t.Fatalf("settings file missing: %v", err)
	}

	pub := settings.PublicView(loaded)
	if pub["capmonster_api_key_set"] != true {
		t.Fatalf("public view missing key flag: %#v", pub)
	}
	if _, ok := pub["capmonster_api_key"]; ok {
		t.Fatal("public view must not expose raw capmonster key")
	}
}

func TestEditorViewIncludesSensitivePlaceholders(t *testing.T) {
	cfg := config.Defaults()
	cfg.CapMonsterAPIKey = "secret"
	view := settings.EditorView(cfg)
	if view == nil {
		t.Fatal("nil view")
	}
}

func TestStoreFillsEmptyProxyAndFlareFromSeed(t *testing.T) {
	dir := t.TempDir()
	// Simulate a stale panel save that wiped proxy/flare while leaving other fields.
	stale := map[string]any{
		"email_provider":       "cfmail",
		"proxy":                "",
		"flaresolverr_url":     "",
		"flaresolverr_enabled": false,
		"total_accounts":       2,
		"max_workers":          2,
	}
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "register_settings.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	seed := config.Defaults()
	seed.Proxy = "http://privoxy:8118"
	seed.FlareSolverrURL = "http://flaresolverr:8191"
	seed.FlareSolverrEnabled = true
	seed.EmailProvider = "mailtm" // should not override non-empty file email_provider

	store, err := settings.NewStore(dir, seed)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	got := store.Get()
	if got.Proxy != "http://privoxy:8118" {
		t.Fatalf("proxy = %q; want seed proxy", got.Proxy)
	}
	if got.FlareSolverrURL != "http://flaresolverr:8191" || !got.FlareSolverrEnabled {
		t.Fatalf("flare = %q enabled=%v", got.FlareSolverrURL, got.FlareSolverrEnabled)
	}
	if got.EmailProvider != "cfmail" {
		t.Fatalf("email_provider = %q; file should win", got.EmailProvider)
	}
	if got.TotalAccounts != 2 || got.MaxWorkers != 2 {
		t.Fatalf("counts = %d/%d", got.TotalAccounts, got.MaxWorkers)
	}

	// Persisted file should now reflect recovered defaults for the panel.
	data, err := os.ReadFile(filepath.Join(dir, "register_settings.json"))
	if err != nil {
		t.Fatalf("read rewritten: %v", err)
	}
	var rewritten config.Config
	if err := json.Unmarshal(data, &rewritten); err != nil {
		t.Fatalf("parse rewritten: %v", err)
	}
	if rewritten.Proxy != "http://privoxy:8118" || rewritten.FlareSolverrURL != "http://flaresolverr:8191" {
		t.Fatalf("rewritten file = proxy=%q flare=%q", rewritten.Proxy, rewritten.FlareSolverrURL)
	}
}

func TestStoreKeepsExplicitFileProxyOverSeed(t *testing.T) {
	dir := t.TempDir()
	stale := map[string]any{
		"proxy":                "http://custom:9999",
		"flaresolverr_url":     "http://custom-flare:8191",
		"flaresolverr_enabled": true,
	}
	raw, _ := json.Marshal(stale)
	_ = os.WriteFile(filepath.Join(dir, "register_settings.json"), append(raw, '\n'), 0o600)

	seed := config.Defaults()
	seed.Proxy = "http://privoxy:8118"
	seed.FlareSolverrURL = "http://flaresolverr:8191"

	store, err := settings.NewStore(dir, seed)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	got := store.Get()
	if got.Proxy != "http://custom:9999" {
		t.Fatalf("proxy = %q; want file value", got.Proxy)
	}
	if got.FlareSolverrURL != "http://custom-flare:8191" {
		t.Fatalf("flare = %q; want file value", got.FlareSolverrURL)
	}
}
