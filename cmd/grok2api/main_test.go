package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
