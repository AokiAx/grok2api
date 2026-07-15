package sqlite_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/security"
	_ "modernc.org/sqlite"
)

func TestSQLiteMigrationCreatesTwoPoolSchema(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "grok2api.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if got := repo.SchemaVersion(ctx); got != 9 {
		t.Fatalf("schema version = %d; want 9", got)
	}
}

func TestLegacyDisabledAccountsMigrateByFailureReason(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database := filepath.Join(dir, "grok2api.db")
	legacy := filepath.Join(dir, "cli_accounts.json")
	payload := map[string]any{
		"accounts": []map[string]any{
			{
				"id":              "quota-account",
				"key":             "quota-token",
				"refresh_token":   "quota-refresh",
				"enabled":         false,
				"fail_count":      5,
				"last_error_code": "subscription:free-usage-exhausted",
			},
			{
				"id":              "auth-account",
				"key":             "auth-token",
				"refresh_token":   "auth-refresh",
				"enabled":         false,
				"fail_count":      5,
				"last_error_code": "invalid-token",
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if _, err := repo.ImportLegacyJSON(ctx, legacy); err != nil {
		t.Fatalf("import legacy: %v", err)
	}

	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("account count = %d; want 3", len(accounts))
	}
	byID := map[string]account.Account{}
	for _, item := range accounts {
		byID[item.ID] = item
	}
	if byID["quota-account"].UnavailableReason != account.ReasonQuota {
		t.Fatalf("quota reason = %q", byID["quota-account"].UnavailableReason)
	}
	if byID["auth-account"].UnavailableReason != account.ReasonAuth {
		t.Fatalf("auth reason = %q", byID["auth-account"].UnavailableReason)
	}
}

func TestLegacyDisabledHeuristicSeparatesExpiredAuthAndQuota(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database := filepath.Join(dir, "grok2api.db")
	legacy := filepath.Join(dir, "cli_accounts.json")
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	payload := map[string]any{
		"accounts": []map[string]any{
			{"id": "expired", "key": "token-expired", "enabled": false, "fail_count": 5, "expires_at": past},
			{"id": "quota", "key": "token-quota", "enabled": false, "fail_count": 5, "expires_at": future},
		},
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if _, err := repo.ImportLegacyJSON(ctx, legacy); err != nil {
		t.Fatalf("import legacy: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	byID := map[string]account.Account{}
	for _, item := range accounts {
		byID[item.ID] = item
	}
	if byID["expired"].UnavailableReason != account.ReasonAuth {
		t.Fatalf("expired reason = %q; want auth", byID["expired"].UnavailableReason)
	}
	if byID["quota"].UnavailableReason != account.ReasonQuota {
		t.Fatalf("quota reason = %q; want quota", byID["quota"].UnavailableReason)
	}
	if byID["quota"].RetryAt.IsZero() {
		t.Fatal("quota account must receive a staggered retry time")
	}
}

func TestSaveAccountPersistsPoolTransitionAndEvent(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "grok2api.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	item := account.Account{
		ID:                "quota-account",
		AccessToken:       "token",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(time.Hour),
		LastErrorCode:     "subscription:free-usage-exhausted",
		QuotaActual:       1_023_321,
		QuotaLimit:        1_000_000,
		MaxActive:         1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save account: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].QuotaActual != 1_023_321 {
		t.Fatalf("saved accounts = %#v", accounts)
	}

	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer db.Close()
	var events int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_state_events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("event count = %d; want 1", events)
	}
}

func TestListAccountsPageAndStats(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "page.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		pool := account.PoolReady
		reason := account.UnavailableReason("")
		if i >= 5 {
			pool = account.PoolUnavailable
			reason = account.ReasonQuota
		}
		id := "acc-" + strconv.Itoa(i)
		item := account.Account{
			ID: id, Email: id + "@ex.com",
			AccessToken: "tok-" + id, Pool: pool, UnavailableReason: reason,
			MaxActive: 2, RequestCount: int64(i + 1), QuotaActual: 10, QuotaLimit: 100,
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now,
		}
		if i == 6 {
			item.LastErrorCode = "refresh-failed"
			item.UnavailableReason = account.ReasonAuth
			item.RefreshToken = "r"
		}
		if err := repo.SaveAccount(ctx, item); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	page, err := repo.ListAccountsPage(ctx, repository.ListAccountsQuery{Page: 2, PageSize: 3, Pool: "ready"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page.Total != 5 || page.Page != 2 || page.PageSize != 3 || len(page.Items) != 2 {
		t.Fatalf("ready page = total=%d page=%d size=%d items=%d", page.Total, page.Page, page.PageSize, len(page.Items))
	}

	search, err := repo.ListAccountsPage(ctx, repository.ListAccountsQuery{Q: "refresh-failed", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if search.Total != 1 || len(search.Items) != 1 || search.Items[0].LastErrorCode != "refresh-failed" {
		t.Fatalf("search = %#v", search)
	}

	stats, err := repo.AccountStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalAccounts != 7 || stats.ReadyAccounts != 5 || stats.UnavailableAccounts != 2 {
		t.Fatalf("pool stats = %#v", stats)
	}
	if stats.MaxActive != 14 || stats.RefreshableAccounts != 1 {
		t.Fatalf("runtime stats = %#v", stats)
	}
	if stats.QuotaObserved != 7 || stats.QuotaRemaining != 7*90 || stats.ReadyQuotaRemaining != 5*90 {
		t.Fatalf("quota stats = %#v", stats)
	}
	if stats.Reasons["quota"] != 1 || stats.Reasons["auth"] != 1 {
		t.Fatalf("reasons = %#v", stats.Reasons)
	}
	if stats.NoRefreshToken != 6 {
		t.Fatalf("no_refresh_token = %d", stats.NoRefreshToken)
	}
	if stats.ErrorCodes["refresh-failed"] != 1 {
		t.Fatalf("error_codes = %#v", stats.ErrorCodes)
	}
}

func TestAccountCount(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "grok2api.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if got, err := repo.AccountCount(ctx); err != nil || got != 0 {
		t.Fatalf("empty count = %d, %v", got, err)
	}
	now := time.Now().UTC()
	if err := repo.SaveAccount(ctx, account.Account{
		ID:          "ready",
		AccessToken: "token",
		Pool:        account.PoolReady,
		MaxActive:   1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	if got, err := repo.AccountCount(ctx); err != nil || got != 1 {
		t.Fatalf("count = %d, %v; want 1", got, err)
	}
}

func TestDeleteAccountRemovesStoredCredential(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	item := account.Account{
		ID: "delete-me", AccessToken: "token", Pool: account.PoolReady,
		CreatedAt: now, UpdatedAt: now, MaxActive: 1,
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.DeleteAccount(ctx, item.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestOpenSQLiteMigratesPythonV1AccountTable(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "python-v1.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatalf("open fixture sqlite: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO app_meta(key, value) VALUES('schema_version', '1');
		CREATE TABLE cli_accounts (
			id TEXT PRIMARY KEY,
			identity_key TEXT NOT NULL UNIQUE,
			key TEXT NOT NULL,
			refresh_token TEXT,
			expires_at TEXT,
			oidc_issuer TEXT NOT NULL,
			oidc_client_id TEXT NOT NULL,
			email TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			request_count INTEGER NOT NULL DEFAULT 0,
			fail_count INTEGER NOT NULL DEFAULT 0,
			cooldown_until REAL,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL,
			disabled_reason TEXT NOT NULL DEFAULT ''
		);
	`)
	if err != nil {
		t.Fatalf("create Python v1 schema: %v", err)
	}
	now := time.Now().UTC()
	rows := []struct {
		id             string
		enabled        int
		failCount      int
		cooldownUntil  any
		disabledReason string
	}{
		{id: "ready", enabled: 1},
		{id: "quota", enabled: 0, failCount: 5, disabledReason: "subscription:free-usage-exhausted"},
		{id: "auth", enabled: 0, failCount: 5, disabledReason: "invalid-token"},
		{id: "cooldown", enabled: 1, cooldownUntil: now.Add(10 * time.Minute).Unix()},
	}
	for _, row := range rows {
		_, err := db.Exec(`INSERT INTO cli_accounts (
			id, identity_key, key, refresh_token, expires_at, oidc_issuer,
			oidc_client_id, email, user_id, enabled, request_count, fail_count,
			cooldown_until, created_at, updated_at, disabled_reason
		) VALUES (?, ?, ?, '', '', 'https://auth.x.ai', 'client', ?, '', ?, 0, ?, ?, ?, ?, ?)`,
			row.id,
			"id:"+row.id,
			"token-"+row.id,
			row.id+"@example.com",
			row.enabled,
			row.failCount,
			row.cooldownUntil,
			now.Add(-time.Hour).Unix(),
			now.Unix(),
			row.disabledReason,
		)
		if err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture sqlite: %v", err)
	}

	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open migrated sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list migrated accounts: %v", err)
	}
	if len(accounts) != len(rows) {
		t.Fatalf("account count = %d; want %d", len(accounts), len(rows))
	}
	byID := make(map[string]account.Account, len(accounts))
	for _, item := range accounts {
		byID[item.ID] = item
	}
	if byID["ready"].Pool != account.PoolReady {
		t.Fatalf("ready = %#v", byID["ready"])
	}
	if byID["quota"].UnavailableReason != account.ReasonQuota || byID["quota"].RetryAt.IsZero() {
		t.Fatalf("quota = %#v", byID["quota"])
	}
	if byID["auth"].UnavailableReason != account.ReasonAuth {
		t.Fatalf("auth = %#v", byID["auth"])
	}
	if byID["cooldown"].UnavailableReason != account.ReasonCooldown || byID["cooldown"].RetryAt.IsZero() {
		t.Fatalf("cooldown = %#v", byID["cooldown"])
	}
	if repo.SchemaVersion(ctx) != 9 {
		t.Fatalf("schema version = %d", repo.SchemaVersion(ctx))
	}
}

func TestLegacyEnabledExpiredCredentialMigratesToAuth(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "cli_accounts.json")
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	data, err := json.Marshal(map[string]any{"accounts": []map[string]any{{
		"id": "expired", "key": "token", "refresh_token": "refresh",
		"enabled": true, "expires_at": past,
	}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if _, err := repo.ImportLegacyJSON(ctx, legacy); err != nil {
		t.Fatalf("import: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].UnavailableReason != account.ReasonAuth {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestSaveAccountPersistsQuotaAndLastSuccess(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "quota.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC().Truncate(time.Millisecond)
	item := account.Account{
		ID:            "free-1",
		AccessToken:   "token",
		Pool:          account.PoolReady,
		QuotaActual:   250_000,
		QuotaLimit:    1_000_000,
		LastSuccessAt: now,
		MaxActive:     1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	// update again to ensure ON CONFLICT persists quota fields
	item.QuotaActual = 300_000
	item.LastSuccessAt = now.Add(time.Minute)
	item.UpdatedAt = now.Add(time.Minute)
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("update: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("count = %d", len(accounts))
	}
	got := accounts[0]
	if got.QuotaActual != 300_000 || got.QuotaLimit != 1_000_000 {
		t.Fatalf("quota = %d/%d", got.QuotaActual, got.QuotaLimit)
	}
	if got.LastSuccessAt.IsZero() {
		t.Fatal("last_success_at not persisted")
	}
}

func TestSaveAccountPersistsTeamID(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	item := account.Account{
		ID:          "team-acc",
		AccessToken: "token",
		Email:       "a@example.com",
		UserID:      "user-1",
		TeamID:      "team-9",
		Pool:        account.PoolReady,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		MaxActive:   1,
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].TeamID != "team-9" {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestImportLegacyJSONKeepsTeamID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "cli_accounts.json")
	payload := map[string]any{"accounts": []map[string]any{{
		"id": "with-team", "key": "token", "email": "t@example.com", "user_id": "u1", "team_id": "team-42", "enabled": true,
	}}}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	count, err := repo.ImportLegacyJSON(ctx, legacy)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil || accounts[0].TeamID != "team-42" {
		t.Fatalf("accounts=%#v err=%v", accounts, err)
	}
}

func TestCredentialEncryptionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc.db")
	// Base64-encoded 32-byte key.
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(rawKey))
	if err != nil || cipher == nil {
		t.Fatalf("cipher: %v", err)
	}
	ctx := context.Background()
	repo, err := sqlite.OpenSQLiteWithCipher(ctx, path, cipher)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	item := account.Account{
		ID: "enc-1", AccessToken: "access-secret", RefreshToken: "refresh-secret",
		Pool: account.PoolReady, MaxActive: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Raw DB should store ciphertext prefix (open without cipher for inspection).
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	var rawAccess string
	if err := db.QueryRowContext(ctx, `SELECT access_token FROM accounts WHERE id=?`, "enc-1").Scan(&rawAccess); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if !security.IsEncrypted(rawAccess) || !strings.HasPrefix(rawAccess, security.EnvelopePrefix) {
		t.Fatalf("want enc:v1: storage, got %q", rawAccess)
	}
	list, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].AccessToken != "access-secret" || list[0].RefreshToken != "refresh-secret" {
		t.Fatalf("decrypted list = %#v", list)
	}
	_ = repo.Close()
}

func TestBase64RefreshTokenDoesNotRequireCredentialKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain-base64.db")
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	refresh := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 64))
	item := account.Account{
		ID: "plain-base64", AccessToken: "access-token", RefreshToken: refresh,
		Pool: account.PoolReady, MaxActive: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	repo, err = sqlite.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("reopen without credential key: %v", err)
	}
	defer repo.Close()
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].RefreshToken != refresh {
		t.Fatalf("refresh token changed: %#v", accounts)
	}
}

func TestOpeningPlaintextDatabaseWithCredentialKeyEncryptsExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "plaintext-to-encrypted.db")
	plainRepo, err := sqlite.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("open plaintext database: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := plainRepo.SaveAccount(ctx, account.Account{
		ID: "migrate-credentials", AccessToken: "plain-access", RefreshToken: "plain-refresh",
		Pool: account.PoolReady, MaxActive: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save plaintext account: %v", err)
	}
	if err := plainRepo.Close(); err != nil {
		t.Fatalf("close plaintext database: %v", err)
	}

	rawKey := bytes.Repeat([]byte{0x2a}, 32)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(rawKey))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	encryptedRepo, err := sqlite.OpenSQLiteWithCipher(ctx, path, cipher)
	if err != nil {
		t.Fatalf("reopen with key: %v", err)
	}
	defer encryptedRepo.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	var rawAccess, rawRefresh string
	if err := db.QueryRowContext(ctx, `SELECT access_token, refresh_token FROM accounts WHERE id=?`, "migrate-credentials").Scan(&rawAccess, &rawRefresh); err != nil {
		t.Fatalf("read raw credentials: %v", err)
	}
	if !security.IsEncrypted(rawAccess) || !security.IsEncrypted(rawRefresh) {
		t.Fatalf("raw credentials not encrypted: access=%q refresh=%q", rawAccess, rawRefresh)
	}
	if !strings.HasPrefix(rawAccess, security.EnvelopePrefix) || !strings.HasPrefix(rawRefresh, security.EnvelopePrefix) {
		t.Fatalf("want enc:v1: envelopes: access=%q refresh=%q", rawAccess, rawRefresh)
	}
	accounts, err := encryptedRepo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list with key: %v", err)
	}
	if len(accounts) != 1 || accounts[0].AccessToken != "plain-access" || accounts[0].RefreshToken != "plain-refresh" {
		t.Fatalf("decrypted accounts = %#v", accounts)
	}
}

func TestEncryptedDatabaseWithWrongCredentialKeyFailsClosedOnRead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wrong-key.db")
	first, err := security.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.OpenSQLiteWithCipher(ctx, path, first)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repo.SaveAccount(ctx, account.Account{
		ID: "wrong-key", AccessToken: "secret-access", RefreshToken: "secret-refresh",
		Pool: account.PoolReady, MaxActive: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := security.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repo, err = sqlite.OpenSQLiteWithCipher(ctx, path, second)
	if err != nil {
		t.Fatalf("open with wrong key: %v", err)
	}
	defer repo.Close()
	if _, err := repo.ListAccounts(ctx); err == nil {
		t.Fatal("expected wrong-key read to fail")
	}
}

func TestLegacyEnvelopeRequiresCredentialKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-envelope.db")
	repo, err := sqlite.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts (
		id, access_token, refresh_token, pool, created_at, updated_at
	) VALUES (?, ?, '', 'ready', ?, ?)`, "legacy-envelope", "enc:v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", now, now); err != nil {
		t.Fatalf("insert legacy envelope: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ListAccounts(ctx); err == nil {
		t.Fatal("expected missing credential key error")
	}
	_ = repo.Close()
}

func TestAccountAdministrationPersistence(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "admin-accounts.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer repo.Close()

	now := time.Date(2026, 7, 15, 6, 30, 0, 0, time.UTC)
	items := []account.Account{
		{ID: "a", AccessToken: "token-a", Pool: account.PoolReady, Priority: 10, MaxActive: 2, CreatedAt: now, UpdatedAt: now},
		{ID: "b", AccessToken: "token-b", Pool: account.PoolReady, Priority: 20, MaxActive: 3, CreatedAt: now, UpdatedAt: now},
	}
	if err := repo.SaveAccounts(ctx, items); err != nil {
		t.Fatalf("save batch: %v", err)
	}
	got, found, err := repo.GetAccount(ctx, "b")
	if err != nil || !found {
		t.Fatalf("get b: found=%v err=%v", found, err)
	}
	if got.Priority != 20 || got.MaxActive != 3 {
		t.Fatalf("runtime controls = %+v", got)
	}

	if err := got.ConfigureRuntime(75, 5, now.Add(time.Minute)); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := repo.SaveAccounts(ctx, []account.Account{got}); err != nil {
		t.Fatalf("save configuration: %v", err)
	}
	events, err := repo.ListAccountEvents(ctx, repository.ListAccountEventsQuery{AccountID: "b", Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if events.Total != 2 || len(events.Items) != 2 {
		t.Fatalf("events = %#v", events)
	}
	latest := events.Items[0]
	if latest.Type != repository.AccountEventConfiguration || latest.Details["priority"] != float64(75) || latest.Details["max_active"] != float64(5) {
		t.Fatalf("latest configuration event = %#v", latest)
	}

	if err := repo.DeleteAccounts(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("delete batch: %v", err)
	}
	if _, found, err := repo.GetAccount(ctx, "a"); err != nil || found {
		t.Fatalf("deleted a: found=%v err=%v", found, err)
	}
}

func TestSaveAccountsRollsBackEntireBatch(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "admin-rollback.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer repo.Close()

	now := time.Now().UTC()
	err = repo.SaveAccounts(ctx, []account.Account{
		{ID: "valid", AccessToken: "token", Pool: account.PoolReady, MaxActive: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "invalid", AccessToken: "token", Pool: account.Pool("broken"), MaxActive: 1, CreatedAt: now, UpdatedAt: now},
	})
	if err == nil {
		t.Fatal("expected invalid batch to fail")
	}
	if _, found, getErr := repo.GetAccount(ctx, "valid"); getErr != nil || found {
		t.Fatalf("valid row escaped rollback: found=%v err=%v", found, getErr)
	}
}

func TestLegacyBareCiphertextIsReadableAndUpgradedOnOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bare-cipher.db")
	rawKey := bytes.Repeat([]byte{0x44}, 32)
	keyCipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(rawKey))
	if err != nil {
		t.Fatal(err)
	}

	// Create schema with a plaintext open, then inject bare ciphertext.
	bootstrap, err := sqlite.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatal(err)
	}

	block, err := aes.NewCipher(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatal(err)
	}
	sealed := aead.Seal(nonce, nonce, []byte("bare-access-token"), nil)
	bare := base64.RawStdEncoding.EncodeToString(sealed)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts (
		id, access_token, refresh_token, pool, created_at, updated_at
	) VALUES (?, ?, '', 'ready', ?, ?)`, "bare-1", bare, now, now); err != nil {
		t.Fatalf("insert bare ciphertext: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := sqlite.OpenSQLiteWithCipher(ctx, path, keyCipher)
	if err != nil {
		t.Fatalf("open with key: %v", err)
	}
	defer repo.Close()

	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].AccessToken != "bare-access-token" {
		t.Fatalf("decoded bare row = %#v", accounts)
	}

	// Open-time migration should rewrite the row under enc:v1:.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	var rawAccess string
	if err := rawDB.QueryRowContext(ctx, `SELECT access_token FROM accounts WHERE id=?`, "bare-1").Scan(&rawAccess); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawAccess, security.EnvelopePrefix) {
		t.Fatalf("expected envelope after open migration, got %q", rawAccess)
	}
}
