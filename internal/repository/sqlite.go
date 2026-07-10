package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	_ "modernc.org/sqlite"
)

const schemaVersion = 2

type SQLite struct {
	db *sql.DB
}

func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	repo := &SQLite{db: db}
	if err := repo.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

func (r *SQLite) Close() error {
	return r.db.Close()
}

func (r *SQLite) migrate(ctx context.Context) error {
	statements := []string{
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
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_pool ON accounts(pool, unavailable_reason, retry_at)`,
		`CREATE TABLE IF NOT EXISTS account_state_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL,
			from_pool TEXT NOT NULL,
			to_pool TEXT NOT NULL,
			reason TEXT NOT NULL,
			error_code TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	if err := r.migratePythonV1(ctx); err != nil {
		return err
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
		if err := upsertAccount(ctx, tx, item); err != nil {
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
			retryAt = now.Add(30*time.Minute + time.Duration(index)*30*time.Second)
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
	ID            string `json:"id"`
	Key           string `json:"key"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	ExpiresAt     string `json:"expires_at"`
	OIDCIssuer    string `json:"oidc_issuer"`
	OIDCClientID  string `json:"oidc_client_id"`
	Email         string `json:"email"`
	UserID        string `json:"user_id"`
	Enabled       *bool  `json:"enabled"`
	RequestCount  int64  `json:"request_count"`
	FailCount     int    `json:"fail_count"`
	LastErrorCode string `json:"last_error_code"`
}

func (r *SQLite) ImportLegacyJSON(ctx context.Context, path string) (int, error) {
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
		item, ok := legacy.toAccount(now, index)
		if !ok {
			continue
		}
		if err := upsertAccount(ctx, tx, item); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit legacy import: %w", err)
	}
	return count, nil
}

func (a legacyAccount) toAccount(now time.Time, index int) (account.Account, bool) {
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
			retryAt = now.Add(30*time.Minute + time.Duration(index)*30*time.Second)
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
		Pool:                pool,
		UnavailableReason:   reason,
		RetryAt:             retryAt,
		LastErrorCode:       a.LastErrorCode,
		RequestCount:        a.RequestCount,
		AuthenticationFails: a.FailCount,
		MaxActive:           1,
		CreatedAt:           now,
		UpdatedAt:           now,
	}, true
}

func (r *SQLite) SaveAccount(ctx context.Context, item account.Account) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save account: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	fromPool := ""
	fromReason := ""
	err = tx.QueryRowContext(
		ctx,
		`SELECT pool, unavailable_reason FROM accounts WHERE id=?`,
		item.ID,
	).Scan(&fromPool, &fromReason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load existing account state: %w", err)
	}
	if err := upsertAccount(ctx, tx, item); err != nil {
		return err
	}
	if fromPool != string(item.Pool) || fromReason != string(item.UnavailableReason) {
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO account_state_events (
				account_id, from_pool, to_pool, reason, error_code, created_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			item.ID,
			fromPool,
			item.Pool,
			item.UnavailableReason,
			item.LastErrorCode,
			time.Now().UTC().Format(time.RFC3339Nano),
		)
		if err != nil {
			return fmt.Errorf("record account state event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account save: %w", err)
	}
	return nil
}

func (r *SQLite) DeleteAccount(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete account: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var fromPool string
	err = tx.QueryRowContext(ctx, `SELECT pool FROM accounts WHERE id=?`, id).Scan(&fromPool)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load account before delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete account %s: %w", id, err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO account_state_events (
			account_id, from_pool, to_pool, reason, error_code, created_at
		) VALUES (?, ?, 'deleted', 'disabled', 'admin-delete', ?)`,
		id,
		fromPool,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record account deletion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account delete: %w", err)
	}
	return nil
}

func upsertAccount(ctx context.Context, tx *sql.Tx, item account.Account) error {
	_, err := tx.ExecContext(
		ctx,
		`INSERT INTO accounts (
			id, access_token, refresh_token, expires_at, oidc_issuer, oidc_client_id,
			email, user_id, pool, unavailable_reason, retry_at, last_error_code,
			last_success_at, quota_actual, quota_limit, request_count,
			authentication_fails, max_active, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			oidc_issuer=excluded.oidc_issuer,
			oidc_client_id=excluded.oidc_client_id,
			email=excluded.email,
			user_id=excluded.user_id,
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
			updated_at=excluded.updated_at`,
		item.ID,
		item.AccessToken,
		item.RefreshToken,
		formatTime(item.ExpiresAt),
		item.OIDCIssuer,
		item.OIDCClientID,
		item.Email,
		item.UserID,
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
		formatTime(item.CreatedAt),
		formatTime(item.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert account %s: %w", item.ID, err)
	}
	return nil
}

func (r *SQLite) ListAccounts(ctx context.Context) ([]account.Account, error) {
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT id, access_token, refresh_token, expires_at, oidc_issuer, oidc_client_id,
			email, user_id, pool, unavailable_reason, retry_at, last_error_code,
			last_success_at, quota_actual, quota_limit, request_count,
			authentication_fails, max_active, created_at, updated_at
		 FROM accounts ORDER BY created_at, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	var result []account.Account
	for rows.Next() {
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
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		item.ExpiresAt = parseTime(expiresAt)
		item.RetryAt = parseTime(retryAt)
		item.LastSuccessAt = parseTime(lastSuccessAt)
		item.CreatedAt = parseTime(createdAt)
		item.UpdatedAt = parseTime(updatedAt)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return result, nil
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
