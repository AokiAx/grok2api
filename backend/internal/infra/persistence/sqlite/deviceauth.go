package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/AokiAx/grok2api/backend/internal/domain/deviceauth"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

var _ repository.DeviceAuthRepository = (*SQLite)(nil)

func (r *SQLite) ensureDeviceAuthSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS device_auth_sessions (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			issuer TEXT NOT NULL,
			client_id TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT '',
			user_code TEXT NOT NULL DEFAULT '',
			verification_uri TEXT NOT NULL DEFAULT '',
			verification_uri_complete TEXT NOT NULL DEFAULT '',
			device_code TEXT NOT NULL DEFAULT '',
			interval_sec INTEGER NOT NULL DEFAULT 5,
			expires_at TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			completed_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_device_auth_status ON device_auth_sessions(status, updated_at)`,
	}
	for _, statement := range statements {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure device auth schema: %w", err)
		}
	}
	return nil
}

func (r *SQLite) CreateDeviceAuthSession(ctx context.Context, item deviceauth.Session) error {
	if err := item.Normalize(); err != nil {
		return err
	}
	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO device_auth_sessions (
			id, status, issuer, client_id, scope, user_code, verification_uri, verification_uri_complete,
			device_code, interval_sec, expires_at, last_error, account_id, created_at, updated_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, string(item.Status), item.Issuer, item.ClientID, item.Scope, item.UserCode,
		item.VerificationURI, item.VerificationURIComplete, item.DeviceCode, item.IntervalSec,
		formatTime(item.ExpiresAt), item.LastError, item.AccountID,
		formatTime(item.CreatedAt), formatTime(item.UpdatedAt), formatTime(item.CompletedAt),
	)
	return err
}

func (r *SQLite) GetDeviceAuthSession(ctx context.Context, id string) (deviceauth.Session, bool, error) {
	row := r.db.QueryRowContext(
		ctx,
		`SELECT id, status, issuer, client_id, scope, user_code, verification_uri, verification_uri_complete,
			device_code, interval_sec, expires_at, last_error, account_id, created_at, updated_at, completed_at
		 FROM device_auth_sessions WHERE id=?`,
		strings.TrimSpace(id),
	)
	item, err := scanDeviceAuth(row)
	if err == sql.ErrNoRows {
		return deviceauth.Session{}, false, nil
	}
	if err != nil {
		return deviceauth.Session{}, false, err
	}
	return item, true, nil
}

func (r *SQLite) UpdateDeviceAuthSession(ctx context.Context, item deviceauth.Session) error {
	if err := item.Normalize(); err != nil {
		return err
	}
	_, err := r.db.ExecContext(
		ctx,
		`UPDATE device_auth_sessions SET
			status=?, issuer=?, client_id=?, scope=?, user_code=?, verification_uri=?, verification_uri_complete=?,
			device_code=?, interval_sec=?, expires_at=?, last_error=?, account_id=?, updated_at=?, completed_at=?
		 WHERE id=?`,
		string(item.Status), item.Issuer, item.ClientID, item.Scope, item.UserCode,
		item.VerificationURI, item.VerificationURIComplete, item.DeviceCode, item.IntervalSec,
		formatTime(item.ExpiresAt), item.LastError, item.AccountID, formatTime(item.UpdatedAt),
		formatTime(item.CompletedAt), item.ID,
	)
	return err
}

func (r *SQLite) ListDeviceAuthSessions(ctx context.Context, limit int) ([]deviceauth.Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT id, status, issuer, client_id, scope, user_code, verification_uri, verification_uri_complete,
			device_code, interval_sec, expires_at, last_error, account_id, created_at, updated_at, completed_at
		 FROM device_auth_sessions
		 ORDER BY created_at DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deviceauth.Session
	for rows.Next() {
		item, err := scanDeviceAuth(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type deviceScanner interface {
	Scan(dest ...any) error
}

func scanDeviceAuth(row deviceScanner) (deviceauth.Session, error) {
	var item deviceauth.Session
	var status, expiresAt, createdAt, updatedAt, completedAt string
	if err := row.Scan(
		&item.ID, &status, &item.Issuer, &item.ClientID, &item.Scope, &item.UserCode,
		&item.VerificationURI, &item.VerificationURIComplete, &item.DeviceCode, &item.IntervalSec,
		&expiresAt, &item.LastError, &item.AccountID, &createdAt, &updatedAt, &completedAt,
	); err != nil {
		return deviceauth.Session{}, err
	}
	item.Status = deviceauth.Status(status)
	item.ExpiresAt = parseTime(expiresAt)
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.CompletedAt = parseTime(completedAt)
	return item, nil
}
