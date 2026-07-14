package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

var _ repository.LegacySecurityBootstrapRepository = (*SQLite)(nil)

func (r *SQLite) BootstrapLegacyAdmin(
	ctx context.Context,
	item adminauth.AdminUser,
) (repository.BootstrapStatus, error) {
	if err := item.Validate(); err != nil {
		return "", fmt.Errorf("validate legacy admin: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin legacy admin bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	marked, err := markerExists(ctx, tx, repository.AdminBootstrapMarker)
	if err != nil {
		return "", err
	}
	if marked {
		return repository.BootstrapAlreadyCompleted, nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return "", fmt.Errorf("count administrators during bootstrap: %w", err)
	}
	status := repository.BootstrapExisting
	if count == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO admin_users(
			id, username, password_scheme, password_hash, role, enabled,
			last_login_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ID, item.Username, item.Password.Scheme, item.Password.Hash, item.Role, item.Enabled,
			formatTime(item.LastLoginAt), formatTime(item.CreatedAt), formatTime(item.UpdatedAt),
		); err != nil {
			return "", fmt.Errorf("insert legacy administrator: %w", err)
		}
		status = repository.BootstrapCreated
	}
	if err := setMarker(ctx, tx, repository.AdminBootstrapMarker, "1"); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit legacy admin bootstrap: %w", err)
	}
	return status, nil
}

func (r *SQLite) BootstrapLegacyClientKey(
	ctx context.Context,
	credential clientkey.Credential,
) (repository.BootstrapStatus, error) {
	validated, err := clientkey.NewCredential(credential.Key, credential.Scopes())
	if err != nil {
		return "", fmt.Errorf("validate legacy client key: %w", err)
	}
	if validated.Key.Origin != clientkey.OriginConfigAPIKey {
		return "", errors.New("legacy client key origin must be config_api_key")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin legacy client key bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	marked, err := markerExists(ctx, tx, repository.ClientKeyBootstrapMarker)
	if err != nil {
		return "", err
	}
	var storedHash []byte
	err = tx.QueryRowContext(ctx, `SELECT key_hash FROM client_keys WHERE origin='config_api_key'`).Scan(&storedHash)
	switch {
	case err == nil:
		if !bytes.Equal(storedHash, validated.Key.KeyHash[:]) {
			return "", errors.New("configured api_key differs from migrated legacy client key")
		}
		if err := setMarker(ctx, tx, repository.ClientKeyBootstrapMarker, "1"); err != nil {
			return "", err
		}
		if err := setMarker(ctx, tx, "client_auth_required", "1"); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit existing legacy client key bootstrap: %w", err)
		}
		if marked {
			return repository.BootstrapAlreadyCompleted, nil
		}
		return repository.BootstrapExisting, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("inspect legacy client key: %w", err)
	}
	if marked {
		if err := setMarker(ctx, tx, "client_auth_required", "1"); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit completed legacy client key bootstrap: %w", err)
		}
		return repository.BootstrapAlreadyCompleted, nil
	}
	if err := insertClientKey(ctx, tx, validated.Key); err != nil {
		return "", fmt.Errorf("insert legacy client key: %w", err)
	}
	if err := replaceClientKeyScopes(ctx, tx, validated.Key.ID, validated.Scopes(), validated.Key.UpdatedAt); err != nil {
		return "", err
	}
	if err := setMarker(ctx, tx, repository.ClientKeyBootstrapMarker, "1"); err != nil {
		return "", err
	}
	if err := setMarker(ctx, tx, "client_auth_required", "1"); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit legacy client key bootstrap: %w", err)
	}
	return repository.BootstrapCreated, nil
}

func markerExists(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read bootstrap marker %s: %w", key, err)
	}
	return value == "1", nil
}

func setMarker(ctx context.Context, tx *sql.Tx, key, value string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
		return fmt.Errorf("write bootstrap marker %s: %w", key, err)
	}
	return nil
}
