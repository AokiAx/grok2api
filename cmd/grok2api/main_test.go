package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/security"
	_ "modernc.org/sqlite"
)

func TestRunMigrateImportsLegacyAccountsAndPrintsPoolCounts(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	legacyPath := filepath.Join(dir, "cli_accounts.json")
	configData, _ := json.Marshal(map[string]any{
		"data_dir": dir,
	})
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	legacyData, _ := json.Marshal(map[string]any{
		"accounts": []map[string]any{
			{"id": "ready", "key": "ready-token", "enabled": true},
			{"id": "quota", "key": "quota-token", "enabled": false, "fail_count": 5, "expires_at": future},
		},
	})
	if err := os.WriteFile(legacyPath, legacyData, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	var output bytes.Buffer
	err := run(
		context.Background(),
		[]string{"migrate", "--config", configPath},
		&output,
	)
	if err != nil {
		t.Fatalf("run migrate: %v", err)
	}
	if !strings.Contains(output.String(), `"ready":1`) {
		t.Fatalf("output = %s", output.String())
	}
	if !strings.Contains(output.String(), `"unavailable":1`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestRunMigrateIsIdempotentAndEncryptsImportedCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	legacyPath := filepath.Join(dir, "cli_accounts.json")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32))
	configData, err := json.Marshal(map[string]any{
		"data_dir":       dir,
		"credential_key": key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	legacyData, err := json.Marshal(map[string]any{
		"accounts": []map[string]any{
			{"id": "encrypted-ready", "key": "ready-secret", "refresh_token": "ready-refresh", "enabled": true},
			{"id": "encrypted-quota", "key": "quota-secret", "enabled": false, "fail_count": 5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, legacyData, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		var output bytes.Buffer
		if err := run(context.Background(), []string{"migrate", "--config", configPath}, &output); err != nil {
			t.Fatalf("migrate attempt %d: %v", attempt, err)
		}
		if !strings.Contains(output.String(), `"ready":1`) || !strings.Contains(output.String(), `"unavailable":1`) {
			t.Fatalf("attempt %d output = %s", attempt, output.String())
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "grok2api.db"))
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT access_token, refresh_token FROM accounts ORDER BY id`)
	if err != nil {
		t.Fatalf("query credentials: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var access, refresh string
		if err := rows.Scan(&access, &refresh); err != nil {
			t.Fatalf("scan credentials: %v", err)
		}
		if !security.IsEncrypted(access) {
			t.Fatalf("access token stored as plaintext: %q", access)
		}
		if refresh != "" && !security.IsEncrypted(refresh) {
			t.Fatalf("refresh token stored as plaintext: %q", refresh)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate credentials: %v", err)
	}
	if count != 2 {
		t.Fatalf("stored account count = %d; want 2", count)
	}
}
