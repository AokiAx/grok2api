package settings_test

import (
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
