package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/security"
	_ "modernc.org/sqlite"
)

const schemaVersion = 10

type SQLite struct {
	db     *sql.DB
	cipher *security.Cipher
}

var _ repository.AccountRepository = (*SQLite)(nil)
var _ repository.AdminAuthRepository = (*SQLite)(nil)
var _ repository.ClientKeyRepository = (*SQLite)(nil)

func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	return OpenSQLiteWithCipher(ctx, path, nil)
}

// OpenSQLiteWithCipher opens the DB and optionally encrypts credentials at rest.
// When cipher is non-nil, tokens are written with the security.EnvelopePrefix
// (enc:v1:) and plaintext or legacy bare-ciphertext rows are migrated on open.
// Credential encryption is opaque to the schema version.
func OpenSQLiteWithCipher(ctx context.Context, path string, cipher *security.Cipher) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	repo := &SQLite{db: db, cipher: cipher}
	if err := repo.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if cipher != nil {
		if err := repo.encryptExistingCredentials(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return repo, nil
}

func (r *SQLite) Close() error {
	return r.db.Close()
}

func (r *SQLite) ForeignKeysEnabled(ctx context.Context) (bool, error) {
	var enabled int
	if err := r.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return false, fmt.Errorf("read sqlite foreign_keys pragma: %w", err)
	}
	return enabled == 1, nil
}

func (r *SQLite) sealToken(value string) (string, error) {
	if r == nil || r.cipher == nil {
		return value, nil
	}
	return r.cipher.Encrypt(value)
}

func (r *SQLite) openToken(value string) (string, error) {
	if r == nil {
		return value, nil
	}
	if r.cipher == nil {
		// Without a key only the explicit envelope is rejected. Bare Base64 is
		// treated as plaintext because OAuth tokens share that shape.
		if security.IsEncrypted(value) {
			return "", fmt.Errorf("encrypted credential requires credential_key")
		}
		return value, nil
	}
	// NormalizeStored opens enc:v1: rows and upgrades legacy bare ciphertext.
	return r.cipher.NormalizeStored(value)
}

func (r *SQLite) encryptExistingCredentials(ctx context.Context) error {
	if r == nil || r.cipher == nil {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, access_token, refresh_token FROM accounts`)
	if err != nil {
		return fmt.Errorf("list credentials for encryption: %w", err)
	}
	defer rows.Close()
	type row struct {
		id, access, refresh string
	}
	var pending []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.id, &item.access, &item.refresh); err != nil {
			return fmt.Errorf("scan credential row: %w", err)
		}
		if security.IsEncrypted(item.access) && (item.refresh == "" || security.IsEncrypted(item.refresh)) {
			continue
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range pending {
		access, err := r.cipher.Encrypt(item.access)
		if err != nil {
			return fmt.Errorf("encrypt access %s: %w", item.id, err)
		}
		refresh, err := r.cipher.Encrypt(item.refresh)
		if err != nil {
			return fmt.Errorf("encrypt refresh %s: %w", item.id, err)
		}
		if access == item.access && refresh == item.refresh {
			continue
		}
		if _, err := r.db.ExecContext(
			ctx,
			`UPDATE accounts SET access_token=?, refresh_token=?, updated_at=? WHERE id=?`,
			access,
			refresh,
			time.Now().UTC().Format(time.RFC3339Nano),
			item.id,
		); err != nil {
			return fmt.Errorf("write encrypted credentials %s: %w", item.id, err)
		}
	}
	return r.encryptExistingDeviceAuthCodes(ctx)
}

func (r *SQLite) encryptExistingDeviceAuthCodes(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT id, device_code FROM device_auth_sessions WHERE device_code != ''`)
	if err != nil {
		return fmt.Errorf("list device auth codes for encryption: %w", err)
	}
	type deviceCodeRow struct {
		id, code string
	}
	var pending []deviceCodeRow
	for rows.Next() {
		var item deviceCodeRow
		if err := rows.Scan(&item.id, &item.code); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan device auth code: %w", err)
		}
		if security.IsEncrypted(item.code) {
			continue
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate device auth codes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close device auth codes: %w", err)
	}
	for _, item := range pending {
		sealed, err := r.cipher.Encrypt(item.code)
		if err != nil {
			return fmt.Errorf("encrypt device auth code %s: %w", item.id, err)
		}
		if _, err := r.db.ExecContext(ctx, `UPDATE device_auth_sessions SET device_code=? WHERE id=?`, sealed, item.id); err != nil {
			return fmt.Errorf("write encrypted device auth code %s: %w", item.id, err)
		}
	}
	return nil
}

func (r *SQLite) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA foreign_keys=ON`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=30000`,
		`CREATE TABLE IF NOT EXISTS app_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			access_token TEXT NOT NULL,
			refresh_token TEXT NOT NULL DEFAULT '',
			expires_at TEXT NOT NULL DEFAULT '',
			oidc_issuer TEXT NOT NULL DEFAULT 'https://auth.x.ai',
			oidc_client_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			team_id TEXT NOT NULL DEFAULT '',
			pool TEXT NOT NULL CHECK(pool IN ('ready','unavailable')),
			unavailable_reason TEXT NOT NULL DEFAULT '',
			retry_at TEXT NOT NULL DEFAULT '',
			last_error_code TEXT NOT NULL DEFAULT '',
			last_success_at TEXT NOT NULL DEFAULT '',
			quota_actual INTEGER NOT NULL DEFAULT 0,
			quota_limit INTEGER NOT NULL DEFAULT 0,
			request_count INTEGER NOT NULL DEFAULT 0,
			authentication_fails INTEGER NOT NULL DEFAULT 0,
			max_active INTEGER NOT NULL DEFAULT 1,
			priority INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_pool ON accounts(pool, unavailable_reason, retry_at)`,
		`CREATE TABLE IF NOT EXISTS account_state_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL,
			from_pool TEXT NOT NULL,
			to_pool TEXT NOT NULL,
			event_type TEXT NOT NULL DEFAULT 'state_transition',
			reason TEXT NOT NULL,
			error_code TEXT NOT NULL DEFAULT '',
			details_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS admin_users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			password_scheme TEXT NOT NULL CHECK(password_scheme = 'bcrypt_sha256_v1'),
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL CHECK(role = 'administrator'),
			enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
			last_login_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS admin_sessions (
			id TEXT PRIMARY KEY,
			family_id TEXT NOT NULL,
			admin_user_id TEXT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
			access_token_hash BLOB NOT NULL UNIQUE CHECK(length(access_token_hash) = 32),
			refresh_secret_hash BLOB NOT NULL UNIQUE CHECK(length(refresh_secret_hash) = 32),
			source_ip TEXT NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT '',
			remember INTEGER NOT NULL DEFAULT 0 CHECK(remember IN (0,1)),
			created_at TEXT NOT NULL,
			access_expires_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			revoked_at TEXT NOT NULL DEFAULT '',
			rotated_at TEXT NOT NULL DEFAULT '',
			replaced_by_session_id TEXT NOT NULL DEFAULT '',
			revocation_reason TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_sessions_family ON admin_sessions(family_id, revoked_at)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_sessions_user ON admin_sessions(admin_user_id, revoked_at, expires_at)`,
		`CREATE TABLE IF NOT EXISTS admin_login_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL COLLATE NOCASE,
			source_ip TEXT NOT NULL DEFAULT '',
			succeeded INTEGER NOT NULL CHECK(succeeded IN (0,1)),
			failure_code TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_login_attempts_username ON admin_login_attempts(username, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_login_attempts_source ON admin_login_attempts(source_ip, created_at)`,
		`CREATE TABLE IF NOT EXISTS admin_login_reservations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL COLLATE NOCASE,
			source_ip TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_login_reservations_username ON admin_login_reservations(username, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_login_reservations_source ON admin_login_reservations(source_ip, created_at)`,
		`CREATE TABLE IF NOT EXISTS client_keys (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			origin TEXT NOT NULL CHECK(origin IN ('managed','config_api_key')),
			key_hash BLOB NOT NULL UNIQUE CHECK(length(key_hash) = 32),
			key_prefix TEXT NOT NULL,
			model_policy TEXT NOT NULL CHECK(model_policy IN ('all','allowlist')),
			rpm_limit INTEGER NOT NULL DEFAULT 0 CHECK(rpm_limit >= 0),
			max_concurrent INTEGER NOT NULL DEFAULT 0 CHECK(max_concurrent >= 0),
			expires_at TEXT NOT NULL DEFAULT '',
			revoked_at TEXT NOT NULL DEFAULT '',
			last_used_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_client_keys_origin ON client_keys(origin, created_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_client_keys_config_origin ON client_keys(origin) WHERE origin='config_api_key'`,
		`CREATE TABLE IF NOT EXISTS client_key_model_scopes (
			client_key_id TEXT NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
			model_id TEXT NOT NULL COLLATE NOCASE,
			created_at TEXT NOT NULL,
			PRIMARY KEY(client_key_id, model_id)
		)`,
		`CREATE TABLE IF NOT EXISTS client_key_rate_windows (
			client_key_id TEXT PRIMARY KEY REFERENCES client_keys(id) ON DELETE CASCADE,
			window_start INTEGER NOT NULL,
			request_count INTEGER NOT NULL CHECK(request_count >= 0),
			last_allowed INTEGER NOT NULL CHECK(last_allowed IN (0,1)),
			updated_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	if err := r.ensureRequestAuditSchema(ctx); err != nil {
		return err
	}
	if err := r.ensureModelRegistrySchema(ctx); err != nil {
		return err
	}
	if err := r.ensureSettingsSchema(ctx); err != nil {
		return err
	}
	if err := r.ensureDeviceAuthSchema(ctx); err != nil {
		return err
	}
	if err := r.ensureAccountColumns(ctx); err != nil {
		return err
	}
	if err := r.ensureAccountEventColumns(ctx); err != nil {
		return err
	}
	if err := r.ensureAdminSessionColumns(ctx); err != nil {
		return err
	}
	if err := r.migratePythonV1(ctx); err != nil {
		return err
	}
	// Fresh installs and upgrades both require client authentication. Operators who
	// intentionally open the gateway must re-disable after migrate; silent '0'
	// from pre-hardening DBs must not survive an upgrade.
	if _, err := r.db.ExecContext(ctx, `INSERT INTO app_meta(key, value) VALUES('client_auth_required', '1')
		ON CONFLICT(key) DO UPDATE SET value='1'`); err != nil {
		return fmt.Errorf("initialize client auth marker: %w", err)
	}
	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO app_meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("%d", schemaVersion),
	)
	if err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return nil
}

func (r *SQLite) ensureAdminSessionColumns(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `PRAGMA table_info(admin_sessions)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return err
		}
		if name == "remember" {
			found = true
		}
	}
	if !found {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE admin_sessions ADD COLUMN remember INTEGER NOT NULL DEFAULT 0 CHECK(remember IN (0,1))`); err != nil {
			return fmt.Errorf("add admin session remember: %w", err)
		}
	}
	return rows.Err()
}

type pythonV1Account struct {
	id             string
	accessToken    string
	refreshToken   sql.NullString
	expiresAt      sql.NullString
	oidcIssuer     string
	oidcClientID   string
	email          string
	userID         string
	enabled        bool
	requestCount   int64
	failCount      int
	cooldownUntil  sql.NullFloat64
	createdAt      float64
	updatedAt      float64
	disabledReason string
}

func (r *SQLite) migratePythonV1(ctx context.Context) error {
	var tableExists int
	if err := r.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cli_accounts'`,
	).Scan(&tableExists); err != nil {
		return fmt.Errorf("inspect Python v1 schema: %w", err)
	}
	if tableExists == 0 {
		return nil
	}
	var accountCount int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&accountCount); err != nil {
		return fmt.Errorf("count Go v2 accounts before migration: %w", err)
	}
	if accountCount > 0 {
		return nil
	}

	rows, err := r.db.QueryContext(
		ctx,
		`SELECT id, key, refresh_token, expires_at, oidc_issuer, oidc_client_id,
			email, user_id, enabled, request_count, fail_count, cooldown_until,
			created_at, updated_at, disabled_reason
		 FROM cli_accounts ORDER BY created_at, id`,
	)
	if err != nil {
		return fmt.Errorf("read Python v1 accounts: %w", err)
	}
	legacy := make([]pythonV1Account, 0)
	for rows.Next() {
		var item pythonV1Account
		if err := rows.Scan(
			&item.id,
			&item.accessToken,
			&item.refreshToken,
			&item.expiresAt,
			&item.oidcIssuer,
			&item.oidcClientID,
			&item.email,
			&item.userID,
			&item.enabled,
			&item.requestCount,
			&item.failCount,
			&item.cooldownUntil,
			&item.createdAt,
			&item.updatedAt,
			&item.disabledReason,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan Python v1 account: %w", err)
		}
		legacy = append(legacy, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate Python v1 accounts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close Python v1 account rows: %w", err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Python v1 migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	for index, legacyItem := range legacy {
		item := legacyItem.toV2(now, index)
		if err := r.upsertAccount(ctx, tx, item); err != nil {
			return fmt.Errorf("migrate Python v1 account %s: %w", item.ID, err)
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO app_meta(key, value) VALUES('python_v1_migrated', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("%d", len(legacy)),
	); err != nil {
		return fmt.Errorf("record Python v1 migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Python v1 migration: %w", err)
	}
	return nil
}

func (a pythonV1Account) toV2(now time.Time, index int) account.Account {
	pool := account.PoolReady
	reason := account.UnavailableReason("")
	retryAt := time.Time{}
	cooldown := time.Time{}
	if a.cooldownUntil.Valid && a.cooldownUntil.Float64 > 0 {
		cooldown = unixFloatTime(a.cooldownUntil.Float64)
	}
	expiresAt := parseTime(a.expiresAt.String)
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		pool = account.PoolUnavailable
		reason = account.ReasonAuth
	} else if a.enabled && cooldown.After(now) {
		pool = account.PoolUnavailable
		reason = account.ReasonCooldown
		retryAt = cooldown
	} else if !a.enabled {
		pool = account.PoolUnavailable
		reason = classifyLegacyDisabledReason(a.disabledReason, a.expiresAt.String, a.failCount, now)
		if reason == account.ReasonQuota {
			retryAt = now.Add(24*time.Hour + time.Duration(index)*30*time.Second)
		} else if reason == account.ReasonCooldown && cooldown.After(now) {
			retryAt = cooldown
		}
	}
	createdAt := unixFloatTime(a.createdAt)
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := unixFloatTime(a.updatedAt)
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	return account.Account{
		ID:                  a.id,
		AccessToken:         a.accessToken,
		RefreshToken:        a.refreshToken.String,
		ExpiresAt:           expiresAt,
		OIDCIssuer:          defaultString(a.oidcIssuer, "https://auth.x.ai"),
		OIDCClientID:        a.oidcClientID,
		Email:               strings.ToLower(strings.TrimSpace(a.email)),
		UserID:              a.userID,
		Pool:                pool,
		UnavailableReason:   reason,
		RetryAt:             retryAt,
		LastErrorCode:       a.disabledReason,
		RequestCount:        a.requestCount,
		AuthenticationFails: a.failCount,
		MaxActive:           1,
		CreatedAt:           createdAt,
		UpdatedAt:           updatedAt,
	}
}

func classifyLegacyDisabledReason(
	disabledReason string,
	expiresAt string,
	failCount int,
	now time.Time,
) account.UnavailableReason {
	message := strings.ToLower(disabledReason)
	switch {
	case strings.Contains(message, "usage-exhausted"),
		strings.Contains(message, "quota"):
		return account.ReasonQuota
	case strings.Contains(message, "token"),
		strings.Contains(message, "auth"),
		strings.Contains(message, "401"),
		strings.Contains(message, "403"),
		strings.Contains(message, "refresh"):
		return account.ReasonAuth
	case strings.Contains(message, "cooldown"), strings.Contains(message, "rate-limit"):
		return account.ReasonCooldown
	case failCount >= 5 && !parseTime(expiresAt).IsZero() && parseTime(expiresAt).Before(now):
		return account.ReasonAuth
	case failCount >= 5:
		return account.ReasonQuota
	default:
		return account.ReasonDisabled
	}
}

func unixFloatTime(value float64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	seconds := int64(value)
	nanoseconds := int64((value - float64(seconds)) * float64(time.Second))
	return time.Unix(seconds, nanoseconds).UTC()
}

func (r *SQLite) SchemaVersion(ctx context.Context) int {
	var raw string
	if err := r.db.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key='schema_version'`).Scan(&raw); err != nil {
		return 0
	}
	var version int
	_, _ = fmt.Sscanf(raw, "%d", &version)
	return version
}

func (r *SQLite) AccountCount(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count accounts: %w", err)
	}
	return count, nil
}

type legacyFile struct {
	Accounts []legacyAccount `json:"accounts"`
}

type legacyAccount struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	TeamID       string `json:"team_id"`
	Enabled      *bool  `json:"enabled"`
	// MaxActive is optional; when omitted/<=0, ImportLegacyJSON uses defaultMaxActive.
	MaxActive     int    `json:"max_active"`
	RequestCount  int64  `json:"request_count"`
	FailCount     int    `json:"fail_count"`
	LastErrorCode string `json:"last_error_code"`
}

// ImportLegacyJSON imports accounts from a legacy cli_accounts.json file.
// defaultMaxActive is used when a record omits max_active or sets it <= 0
// (same policy as admin API import / global pool.max_concurrent).
// Pass <=0 to fall back to 1.
func (r *SQLite) ImportLegacyJSON(ctx context.Context, path string, defaultMaxActive int) (int, error) {
	if defaultMaxActive <= 0 {
		defaultMaxActive = 1
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read legacy accounts: %w", err)
	}
	var payload legacyFile
	if err := json.Unmarshal(data, &payload); err != nil {
		var accounts []legacyAccount
		if arrayErr := json.Unmarshal(data, &accounts); arrayErr != nil {
			return 0, fmt.Errorf("parse legacy accounts: %w", err)
		}
		payload.Accounts = accounts
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin legacy import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	count := 0
	now := time.Now().UTC()
	for index, legacy := range payload.Accounts {
		item, ok := legacy.toAccount(now, index, defaultMaxActive)
		if !ok {
			continue
		}
		if err := r.upsertAccount(ctx, tx, item); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit legacy import: %w", err)
	}
	return count, nil
}

func (a legacyAccount) toAccount(now time.Time, index int, defaultMaxActive int) (account.Account, bool) {
	token := strings.TrimSpace(a.Key)
	if token == "" {
		token = strings.TrimSpace(a.AccessToken)
	}
	if token == "" {
		return account.Account{}, false
	}
	id := strings.TrimSpace(a.ID)
	if id == "" {
		id = strings.ToLower(strings.TrimSpace(a.Email))
	}
	if id == "" {
		id = fmt.Sprintf("legacy-%x", []byte(token)[:min(8, len(token))])
	}

	pool := account.PoolReady
	reason := account.UnavailableReason("")
	retryAt := time.Time{}
	expiresAt := parseTime(a.ExpiresAt)
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		pool = account.PoolUnavailable
		reason = account.ReasonAuth
	} else if a.Enabled != nil && !*a.Enabled {
		pool = account.PoolUnavailable
		switch {
		case strings.Contains(strings.ToLower(a.LastErrorCode), "usage-exhausted"):
			reason = account.ReasonQuota
		case strings.Contains(strings.ToLower(a.LastErrorCode), "token"), strings.Contains(strings.ToLower(a.LastErrorCode), "auth"):
			reason = account.ReasonAuth
		case a.FailCount >= 5 && !parseTime(a.ExpiresAt).IsZero() && parseTime(a.ExpiresAt).Before(now):
			reason = account.ReasonAuth
		case a.FailCount >= 5:
			reason = account.ReasonQuota
		default:
			reason = account.ReasonDisabled
		}
		if reason == account.ReasonQuota {
			retryAt = now.Add(24*time.Hour + time.Duration(index)*30*time.Second)
		}
	}
	return account.Account{
		ID:                  id,
		AccessToken:         token,
		RefreshToken:        a.RefreshToken,
		ExpiresAt:           expiresAt,
		OIDCIssuer:          defaultString(a.OIDCIssuer, "https://auth.x.ai"),
		OIDCClientID:        a.OIDCClientID,
		Email:               strings.ToLower(strings.TrimSpace(a.Email)),
		UserID:              a.UserID,
		TeamID:              strings.TrimSpace(a.TeamID),
		Pool:                pool,
		UnavailableReason:   reason,
		RetryAt:             retryAt,
		LastErrorCode:       a.LastErrorCode,
		RequestCount:        a.RequestCount,
		AuthenticationFails: a.FailCount,
		MaxActive:           resolveLegacyMaxActive(a.MaxActive, defaultMaxActive),
		CreatedAt:           now,
		UpdatedAt:           now,
	}, true
}

func resolveLegacyMaxActive(explicit, defaultMaxActive int) int {
	if explicit > 0 {
		return explicit
	}
	if defaultMaxActive > 0 {
		return defaultMaxActive
	}
	return 1
}

func (r *SQLite) SaveAccount(ctx context.Context, item account.Account) error {
	return r.SaveAccounts(ctx, []account.Account{item})
}

func (r *SQLite) SaveAccounts(ctx context.Context, items []account.Account) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save accounts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range items {
		if err := r.saveAccountTx(ctx, tx, item); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account saves: %w", err)
	}
	return nil
}

func (r *SQLite) DeleteAccount(ctx context.Context, id string) error {
	return r.DeleteAccounts(ctx, []string{id})
}

func (r *SQLite) DeleteAccounts(ctx context.Context, ids []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete accounts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if err := r.deleteAccountTx(ctx, tx, strings.TrimSpace(id)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account deletes: %w", err)
	}
	return nil
}

func (r *SQLite) saveAccountTx(ctx context.Context, tx *sql.Tx, item account.Account) error {
	var fromPool, fromReason string
	var fromPriority, fromMaxActive int
	err := tx.QueryRowContext(ctx, `SELECT pool, unavailable_reason, priority, max_active FROM accounts WHERE id=?`, item.ID).
		Scan(&fromPool, &fromReason, &fromPriority, &fromMaxActive)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load existing account state: %w", err)
	}
	if err := r.upsertAccount(ctx, tx, item); err != nil {
		return err
	}
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if !exists || fromPool != string(item.Pool) || fromReason != string(item.UnavailableReason) {
		if err := insertAccountEvent(ctx, tx, item.ID, fromPool, string(item.Pool), repository.AccountEventStateTransition, string(item.UnavailableReason), item.LastErrorCode, nil, createdAt); err != nil {
			return err
		}
	}
	if exists && (fromPriority != item.Priority || fromMaxActive != item.MaxActive) {
		details := map[string]any{"priority": item.Priority, "max_active": item.MaxActive}
		if err := insertAccountEvent(ctx, tx, item.ID, fromPool, string(item.Pool), repository.AccountEventConfiguration, "admin-update", "", details, createdAt); err != nil {
			return err
		}
	}
	return nil
}

func (r *SQLite) deleteAccountTx(ctx context.Context, tx *sql.Tx, id string) error {
	if id == "" {
		return nil
	}
	var fromPool string
	err := tx.QueryRowContext(ctx, `SELECT pool FROM accounts WHERE id=?`, id).Scan(&fromPool)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load account before delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete account %s: %w", id, err)
	}
	return insertAccountEvent(ctx, tx, id, fromPool, "deleted", repository.AccountEventDeletion, "disabled", "admin-delete", nil, time.Now().UTC().Format(time.RFC3339Nano))
}

func insertAccountEvent(ctx context.Context, tx *sql.Tx, accountID, fromPool, toPool string, eventType repository.AccountEventType, reason, errorCode string, details map[string]any, createdAt string) error {
	if details == nil {
		details = map[string]any{}
	}
	rawDetails, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode account event details: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO account_state_events (
		account_id, from_pool, to_pool, event_type, reason, error_code, details_json, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, accountID, fromPool, toPool, eventType, reason, errorCode, string(rawDetails), createdAt)
	if err != nil {
		return fmt.Errorf("record account event: %w", err)
	}
	return nil
}

func (r *SQLite) upsertAccount(ctx context.Context, tx *sql.Tx, item account.Account) error {
	access, err := r.sealToken(item.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt access_token: %w", err)
	}
	refresh, err := r.sealToken(item.RefreshToken)
	if err != nil {
		return fmt.Errorf("encrypt refresh_token: %w", err)
	}
	item.AccessToken = access
	item.RefreshToken = refresh
	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO accounts (
			id, access_token, refresh_token, expires_at, oidc_issuer, oidc_client_id,
			email, user_id, team_id, pool, unavailable_reason, retry_at, last_error_code,
			last_success_at, quota_actual, quota_limit, request_count,
			authentication_fails, max_active, priority, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			oidc_issuer=excluded.oidc_issuer,
			oidc_client_id=excluded.oidc_client_id,
			email=excluded.email,
			user_id=excluded.user_id,
			team_id=excluded.team_id,
			pool=excluded.pool,
			unavailable_reason=excluded.unavailable_reason,
			retry_at=excluded.retry_at,
			last_error_code=excluded.last_error_code,
			last_success_at=excluded.last_success_at,
			quota_actual=excluded.quota_actual,
			quota_limit=excluded.quota_limit,
			request_count=excluded.request_count,
			authentication_fails=excluded.authentication_fails,
			max_active=excluded.max_active,
			priority=excluded.priority,
			updated_at=excluded.updated_at`,
		item.ID,
		item.AccessToken,
		item.RefreshToken,
		formatTime(item.ExpiresAt),
		item.OIDCIssuer,
		item.OIDCClientID,
		item.Email,
		item.UserID,
		item.TeamID,
		item.Pool,
		item.UnavailableReason,
		formatTime(item.RetryAt),
		item.LastErrorCode,
		formatTime(item.LastSuccessAt),
		item.QuotaActual,
		item.QuotaLimit,
		item.RequestCount,
		item.AuthenticationFails,
		item.MaxActive,
		item.Priority,
		formatTime(item.CreatedAt),
		formatTime(item.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert account %s: %w", item.ID, err)
	}
	return nil
}

const (
	defaultListPageSize = 50
	maxListPageSize     = 200
)

func normalizeListQuery(query repository.ListAccountsQuery) repository.ListAccountsQuery {
	page := query.Page
	if page < 1 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = defaultListPageSize
	}
	if pageSize > maxListPageSize {
		pageSize = maxListPageSize
	}
	pool := strings.ToLower(strings.TrimSpace(query.Pool))
	if pool != "ready" && pool != "unavailable" {
		pool = ""
	}
	return repository.ListAccountsQuery{
		Pool:     pool,
		Q:        strings.TrimSpace(query.Q),
		Page:     page,
		PageSize: pageSize,
	}
}

func accountListWhere(query repository.ListAccountsQuery) (string, []any) {
	var clauses []string
	var args []any
	if query.Pool != "" {
		clauses = append(clauses, "pool = ?")
		args = append(args, query.Pool)
	}
	if query.Q != "" {
		like := "%" + escapeLike(query.Q) + "%"
		clauses = append(clauses,
			`(id LIKE ? ESCAPE '\' OR email LIKE ? ESCAPE '\' OR unavailable_reason LIKE ? ESCAPE '\' OR last_error_code LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

func (r *SQLite) ListAccounts(ctx context.Context) ([]account.Account, error) {
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT id, access_token, refresh_token, expires_at, oidc_issuer, oidc_client_id,
			email, user_id, team_id, pool, unavailable_reason, retry_at, last_error_code,
			last_success_at, quota_actual, quota_limit, request_count,
			authentication_fails, max_active, priority, created_at, updated_at
		 FROM accounts ORDER BY created_at, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	var result []account.Account
	for rows.Next() {
		item, err := r.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return result, nil
}

func (r *SQLite) ListAccountsPage(
	ctx context.Context,
	query repository.ListAccountsQuery,
) (repository.ListAccountsResult, error) {
	query = normalizeListQuery(query)
	where, args := accountListWhere(query)

	var total int
	countSQL := `SELECT COUNT(*) FROM accounts ` + where
	if err := r.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return repository.ListAccountsResult{}, fmt.Errorf("count filtered accounts: %w", err)
	}

	offset := (query.Page - 1) * query.PageSize
	listSQL := `SELECT id, access_token, refresh_token, expires_at, oidc_issuer, oidc_client_id,
			email, user_id, team_id, pool, unavailable_reason, retry_at, last_error_code,
			last_success_at, quota_actual, quota_limit, request_count,
			authentication_fails, max_active, priority, created_at, updated_at
		 FROM accounts ` + where + ` ORDER BY created_at, id LIMIT ? OFFSET ?`
	listArgs := append(append([]any{}, args...), query.PageSize, offset)
	rows, err := r.db.QueryContext(ctx, listSQL, listArgs...)
	if err != nil {
		return repository.ListAccountsResult{}, fmt.Errorf("list accounts page: %w", err)
	}
	defer rows.Close()

	items := make([]account.Account, 0, query.PageSize)
	for rows.Next() {
		item, err := r.scanAccount(rows)
		if err != nil {
			return repository.ListAccountsResult{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return repository.ListAccountsResult{}, fmt.Errorf("iterate accounts page: %w", err)
	}
	return repository.ListAccountsResult{
		Items:    items,
		Total:    total,
		Page:     query.Page,
		PageSize: query.PageSize,
	}, nil
}

type accountScanner interface {
	Scan(dest ...any) error
}

func (r *SQLite) scanAccount(rows accountScanner) (account.Account, error) {
	var item account.Account
	var expiresAt, retryAt, lastSuccessAt, createdAt, updatedAt string
	if err := rows.Scan(
		&item.ID,
		&item.AccessToken,
		&item.RefreshToken,
		&expiresAt,
		&item.OIDCIssuer,
		&item.OIDCClientID,
		&item.Email,
		&item.UserID,
		&item.TeamID,
		&item.Pool,
		&item.UnavailableReason,
		&retryAt,
		&item.LastErrorCode,
		&lastSuccessAt,
		&item.QuotaActual,
		&item.QuotaLimit,
		&item.RequestCount,
		&item.AuthenticationFails,
		&item.MaxActive,
		&item.Priority,
		&createdAt,
		&updatedAt,
	); err != nil {
		return account.Account{}, fmt.Errorf("scan account: %w", err)
	}
	item.ExpiresAt = parseTime(expiresAt)
	item.RetryAt = parseTime(retryAt)
	item.LastSuccessAt = parseTime(lastSuccessAt)
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	access, err := r.openToken(item.AccessToken)
	if err != nil {
		return account.Account{}, fmt.Errorf("decrypt access_token %s: %w", item.ID, err)
	}
	refresh, err := r.openToken(item.RefreshToken)
	if err != nil {
		return account.Account{}, fmt.Errorf("decrypt refresh_token %s: %w", item.ID, err)
	}
	item.AccessToken = access
	item.RefreshToken = refresh
	return item, nil
}

func (r *SQLite) GetAccount(ctx context.Context, id string) (account.Account, bool, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, access_token, refresh_token, expires_at, oidc_issuer, oidc_client_id,
		email, user_id, team_id, pool, unavailable_reason, retry_at, last_error_code,
		last_success_at, quota_actual, quota_limit, request_count,
		authentication_fails, max_active, priority, created_at, updated_at
		FROM accounts WHERE id=?`, strings.TrimSpace(id))
	item, err := r.scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return account.Account{}, false, nil
	}
	if err != nil {
		return account.Account{}, false, err
	}
	return item, true, nil
}

func (r *SQLite) ListAccountEvents(ctx context.Context, query repository.ListAccountEventsQuery) (repository.ListAccountEventsResult, error) {
	query.AccountID = strings.TrimSpace(query.AccountID)
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 20
	}
	if query.PageSize > 200 {
		query.PageSize = 200
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_state_events WHERE account_id=?`, query.AccountID).Scan(&total); err != nil {
		return repository.ListAccountEventsResult{}, fmt.Errorf("count account events: %w", err)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, account_id, event_type, from_pool, to_pool, reason, error_code, details_json, created_at
		FROM account_state_events WHERE account_id=? ORDER BY id DESC LIMIT ? OFFSET ?`, query.AccountID, query.PageSize, (query.Page-1)*query.PageSize)
	if err != nil {
		return repository.ListAccountEventsResult{}, fmt.Errorf("list account events: %w", err)
	}
	defer rows.Close()
	items := make([]repository.AccountEvent, 0, query.PageSize)
	for rows.Next() {
		var item repository.AccountEvent
		var eventType, fromPool, toPool, detailsJSON, createdAt string
		if err := rows.Scan(&item.ID, &item.AccountID, &eventType, &fromPool, &toPool, &item.Reason, &item.ErrorCode, &detailsJSON, &createdAt); err != nil {
			return repository.ListAccountEventsResult{}, fmt.Errorf("scan account event: %w", err)
		}
		item.Type = repository.AccountEventType(eventType)
		item.FromPool = account.Pool(fromPool)
		item.ToPool = account.Pool(toPool)
		item.CreatedAt = parseTime(createdAt)
		item.Details = map[string]any{}
		if detailsJSON != "" {
			if err := json.Unmarshal([]byte(detailsJSON), &item.Details); err != nil {
				return repository.ListAccountEventsResult{}, fmt.Errorf("decode account event details: %w", err)
			}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return repository.ListAccountEventsResult{}, fmt.Errorf("iterate account events: %w", err)
	}
	return repository.ListAccountEventsResult{Items: items, Total: total, Page: query.Page, PageSize: query.PageSize}, nil
}

// AccountStats returns global pool aggregates without loading token rows.
func (r *SQLite) AccountStats(ctx context.Context) (repository.AccountStats, error) {
	stats := repository.AccountStats{
		Reasons:    make(map[string]int),
		ErrorCodes: make(map[string]int),
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	soon := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN pool = 'ready' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN pool = 'unavailable' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(CASE WHEN max_active > 0 THEN max_active ELSE 1 END), 0),
			COALESCE(SUM(CASE WHEN refresh_token != '' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN quota_limit > 0 THEN
					CASE
						WHEN quota_actual < 0 THEN 0
						WHEN quota_actual > quota_limit THEN quota_limit
						ELSE quota_actual
					END
				ELSE 0
			END), 0),
			COALESCE(SUM(CASE WHEN quota_limit > 0 THEN quota_limit ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN quota_limit > 0 THEN
					quota_limit - CASE
						WHEN quota_actual < 0 THEN 0
						WHEN quota_actual > quota_limit THEN quota_limit
						ELSE quota_actual
					END
				ELSE 0
			END), 0),
			COALESCE(SUM(CASE
				WHEN pool = 'ready' AND quota_limit > 0 THEN
					quota_limit - CASE
						WHEN quota_actual < 0 THEN 0
						WHEN quota_actual > quota_limit THEN quota_limit
						ELSE quota_actual
					END
				ELSE 0
			END), 0),
			COALESCE(SUM(CASE WHEN quota_limit > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN pool = 'ready' AND quota_limit > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN authentication_fails > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(authentication_fails), 0),
			COALESCE(SUM(CASE WHEN expires_at != '' AND expires_at < ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN expires_at != '' AND expires_at >= ? AND expires_at < ? THEN 1 ELSE 0
			END), 0),
			COALESCE(SUM(CASE
				WHEN pool = 'unavailable' AND retry_at != '' AND retry_at <= ? THEN 1 ELSE 0
			END), 0),
			COALESCE(SUM(CASE WHEN refresh_token = '' THEN 1 ELSE 0 END), 0)
		FROM accounts`, now, now, soon, now).Scan(
		&stats.TotalAccounts,
		&stats.ReadyAccounts,
		&stats.UnavailableAccounts,
		&stats.TotalRequests,
		&stats.MaxActive,
		&stats.RefreshableAccounts,
		&stats.QuotaActual,
		&stats.QuotaLimit,
		&stats.QuotaRemaining,
		&stats.ReadyQuotaRemaining,
		&stats.QuotaObserved,
		&stats.ReadyQuotaObserved,
		&stats.AuthFailAccounts,
		&stats.TotalAuthFails,
		&stats.AccessExpired,
		&stats.AccessExpiringSoon,
		&stats.RetryDue,
		&stats.NoRefreshToken,
	)
	if err != nil {
		return repository.AccountStats{}, fmt.Errorf("account stats: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT unavailable_reason, COUNT(*)
		FROM accounts
		WHERE pool = 'unavailable' AND unavailable_reason != ''
		GROUP BY unavailable_reason`)
	if err != nil {
		return repository.AccountStats{}, fmt.Errorf("account reason stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var count int
		if err := rows.Scan(&reason, &count); err != nil {
			return repository.AccountStats{}, fmt.Errorf("scan reason stats: %w", err)
		}
		stats.Reasons[reason] = count
	}
	if err := rows.Err(); err != nil {
		return repository.AccountStats{}, fmt.Errorf("iterate reason stats: %w", err)
	}

	codeRows, err := r.db.QueryContext(ctx, `
		SELECT last_error_code, COUNT(*)
		FROM accounts
		WHERE last_error_code != ''
		GROUP BY last_error_code
		ORDER BY COUNT(*) DESC
		LIMIT 20`)
	if err != nil {
		return repository.AccountStats{}, fmt.Errorf("account error code stats: %w", err)
	}
	defer codeRows.Close()
	for codeRows.Next() {
		var code string
		var count int
		if err := codeRows.Scan(&code, &count); err != nil {
			return repository.AccountStats{}, fmt.Errorf("scan error code stats: %w", err)
		}
		stats.ErrorCodes[code] = count
	}
	if err := codeRows.Err(); err != nil {
		return repository.AccountStats{}, fmt.Errorf("iterate error code stats: %w", err)
	}
	return stats, nil
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (r *SQLite) ensureAccountColumns(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `PRAGMA table_info(accounts)`)
	if err != nil {
		return fmt.Errorf("inspect accounts columns: %w", err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan accounts column: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close accounts columns: %w", err)
	}
	if !columns["team_id"] {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN team_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add team_id column: %w", err)
		}
	}
	if !columns["priority"] {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add priority column: %w", err)
		}
	}
	return nil
}

func (r *SQLite) ensureAccountEventColumns(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `PRAGMA table_info(account_state_events)`)
	if err != nil {
		return fmt.Errorf("inspect account event columns: %w", err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan account event column: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close account event columns: %w", err)
	}
	if !columns["event_type"] {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE account_state_events ADD COLUMN event_type TEXT NOT NULL DEFAULT 'state_transition'`); err != nil {
			return fmt.Errorf("add account event type column: %w", err)
		}
	}
	if !columns["details_json"] {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE account_state_events ADD COLUMN details_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
			return fmt.Errorf("add account event details column: %w", err)
		}
	}
	return nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
