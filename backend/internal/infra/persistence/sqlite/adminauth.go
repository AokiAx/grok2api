package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
)

func (r *SQLite) CountAdminUsers(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count admin users: %w", err)
	}
	return count, nil
}

func (r *SQLite) CreateAdminUser(ctx context.Context, item adminauth.AdminUser) error {
	if err := item.Validate(); err != nil {
		return fmt.Errorf("validate admin user: %w", err)
	}
	if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return errors.New("admin user timestamps are required")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO admin_users(
		id, username, password_scheme, password_hash, role, enabled,
		last_login_at, created_at, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID,
		item.Username,
		item.Password.Scheme,
		item.Password.Hash,
		item.Role,
		item.Enabled,
		formatTime(item.LastLoginAt),
		formatTime(item.CreatedAt),
		formatTime(item.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("create admin user %s: %w", item.ID, err)
	}
	return nil
}

func (r *SQLite) GetAdminUserByID(ctx context.Context, id string) (adminauth.AdminUser, bool, error) {
	return scanAdminUser(r.db.QueryRowContext(ctx, `SELECT
		id, username, password_scheme, password_hash, role, enabled,
		last_login_at, created_at, updated_at
		FROM admin_users WHERE id=?`, strings.TrimSpace(id)))
}

func (r *SQLite) GetAdminUserByUsername(ctx context.Context, username string) (adminauth.AdminUser, bool, error) {
	return scanAdminUser(r.db.QueryRowContext(ctx, `SELECT
		id, username, password_scheme, password_hash, role, enabled,
		last_login_at, created_at, updated_at
		FROM admin_users WHERE username=?`, strings.ToLower(strings.TrimSpace(username))))
}

func scanAdminUser(row rowScanner) (adminauth.AdminUser, bool, error) {
	var item adminauth.AdminUser
	var enabled int
	var lastLoginAt, createdAt, updatedAt string
	if err := row.Scan(
		&item.ID,
		&item.Username,
		&item.Password.Scheme,
		&item.Password.Hash,
		&item.Role,
		&enabled,
		&lastLoginAt,
		&createdAt,
		&updatedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return adminauth.AdminUser{}, false, nil
	} else if err != nil {
		return adminauth.AdminUser{}, false, fmt.Errorf("scan admin user: %w", err)
	}
	item.Enabled = enabled == 1
	item.LastLoginAt = parseTime(lastLoginAt)
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	return item, true, nil
}

func (r *SQLite) CreateAdminSession(ctx context.Context, item adminauth.Session) error {
	if err := validateSession(item); err != nil {
		return err
	}
	if err := insertAdminSession(ctx, r.db, item); err != nil {
		return fmt.Errorf("create admin session %s: %w", item.ID, err)
	}
	return nil
}

func (r *SQLite) GetAdminSession(ctx context.Context, id string) (adminauth.Session, bool, error) {
	return scanAdminSession(r.db.QueryRowContext(ctx, sessionSelect+` WHERE id=?`, strings.TrimSpace(id)))
}

func (r *SQLite) FindAdminSessionByAccessHash(ctx context.Context, hash [32]byte) (adminauth.Session, bool, error) {
	if hash == ([32]byte{}) {
		return adminauth.Session{}, false, errors.New("access token hash is required")
	}
	return scanAdminSession(r.db.QueryRowContext(ctx, sessionSelect+` WHERE access_token_hash=?`, hash[:]))
}

func (r *SQLite) RotateAdminSession(
	ctx context.Context,
	sessionID string,
	expectedRefreshSecretHash [32]byte,
	replacement adminauth.Session,
	at time.Time,
) (bool, error) {
	if at.IsZero() {
		return false, errors.New("session rotation time is required")
	}
	if expectedRefreshSecretHash == ([32]byte{}) {
		return false, errors.New("refresh secret hash is required")
	}
	if err := validateSession(replacement); err != nil {
		return false, err
	}
	at = at.UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin admin session rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `UPDATE admin_sessions SET
		rotated_at=?, revoked_at=?, revocation_reason=?, replaced_by_session_id=?, last_seen_at=?
		WHERE id=? AND refresh_secret_hash=? AND family_id=? AND admin_user_id=?
		AND revoked_at='' AND rotated_at='' AND expires_at>?`,
		formatTime(at),
		formatTime(at),
		adminauth.RevocationRotated,
		replacement.ID,
		formatTime(at),
		strings.TrimSpace(sessionID),
		expectedRefreshSecretHash[:],
		replacement.FamilyID,
		replacement.AdminUserID,
		formatTime(at),
	)
	if err != nil {
		return false, fmt.Errorf("rotate admin session %s: %w", sessionID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect admin session rotation: %w", err)
	}
	if rowsAffected == 0 {
		return false, nil
	}
	if err := insertAdminSession(ctx, tx, replacement); err != nil {
		return false, fmt.Errorf("create replacement admin session %s: %w", replacement.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit admin session rotation: %w", err)
	}
	return true, nil
}

func (r *SQLite) RevokeAdminSession(
	ctx context.Context,
	sessionID string,
	at time.Time,
	reason adminauth.RevocationReason,
) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session id is required")
	}
	if at.IsZero() {
		return errors.New("session revocation time is required")
	}
	if reason == "" {
		return errors.New("session revocation reason is required")
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE admin_sessions SET revoked_at=?, revocation_reason=?
		WHERE id=? AND revoked_at=''`, formatTime(at), reason, strings.TrimSpace(sessionID)); err != nil {
		return fmt.Errorf("revoke admin session %s: %w", sessionID, err)
	}
	return nil
}

func (r *SQLite) RevokeAdminSessionFamily(
	ctx context.Context,
	familyID string,
	at time.Time,
	reason adminauth.RevocationReason,
) error {
	if strings.TrimSpace(familyID) == "" {
		return errors.New("session family id is required")
	}
	if at.IsZero() {
		return errors.New("session family revocation time is required")
	}
	if reason == "" {
		return errors.New("session family revocation reason is required")
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE admin_sessions SET revoked_at=?, revocation_reason=?
		WHERE family_id=? AND revoked_at=''`, formatTime(at), reason, strings.TrimSpace(familyID)); err != nil {
		return fmt.Errorf("revoke admin session family %s: %w", familyID, err)
	}
	return nil
}

func (r *SQLite) RecordAdminLoginAttempt(ctx context.Context, item adminauth.LoginAttempt) error {
	if err := item.Validate(); err != nil {
		return fmt.Errorf("validate admin login attempt: %w", err)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO admin_login_attempts(
		username, source_ip, succeeded, failure_code, created_at
	) VALUES(?, ?, ?, ?, ?)`,
		strings.ToLower(strings.TrimSpace(item.Username)),
		strings.TrimSpace(item.SourceIP),
		item.Succeeded,
		item.FailureCode,
		formatTime(item.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("record admin login attempt: %w", err)
	}
	return nil
}

func (r *SQLite) CountRecentAdminLoginFailures(
	ctx context.Context,
	username, sourceIP string,
	since time.Time,
) (int, error) {
	if since.IsZero() {
		return 0, errors.New("login failure window start is required")
	}
	var count int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_login_attempts
		WHERE username=? AND source_ip=? AND succeeded=0 AND created_at>=?`,
		strings.ToLower(strings.TrimSpace(username)),
		strings.TrimSpace(sourceIP),
		formatTime(since),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count recent admin login failures: %w", err)
	}
	return count, nil
}

const sessionSelect = `SELECT
	id, family_id, admin_user_id, access_token_hash, refresh_secret_hash,
	source_ip, user_agent, created_at, access_expires_at, expires_at,
	last_seen_at, revoked_at, rotated_at, replaced_by_session_id, revocation_reason
	FROM admin_sessions`

type rowScanner interface {
	Scan(...any) error
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func validateSession(item adminauth.Session) error {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.FamilyID) == "" || strings.TrimSpace(item.AdminUserID) == "" {
		return errors.New("admin session identity is required")
	}
	if item.AccessTokenHash == ([32]byte{}) || item.RefreshSecretHash == ([32]byte{}) {
		return errors.New("admin session hashes are required")
	}
	if item.CreatedAt.IsZero() || item.AccessExpiresAt.IsZero() || item.ExpiresAt.IsZero() || item.LastSeenAt.IsZero() {
		return errors.New("admin session timestamps are required")
	}
	if !item.AccessExpiresAt.After(item.CreatedAt) || !item.ExpiresAt.After(item.CreatedAt) || item.AccessExpiresAt.After(item.ExpiresAt) {
		return errors.New("admin session expiry is invalid")
	}
	return nil
}

func insertAdminSession(ctx context.Context, execer contextExecer, item adminauth.Session) error {
	_, err := execer.ExecContext(ctx, `INSERT INTO admin_sessions(
		id, family_id, admin_user_id, access_token_hash, refresh_secret_hash,
		source_ip, user_agent, created_at, access_expires_at, expires_at,
		last_seen_at, revoked_at, rotated_at, replaced_by_session_id, revocation_reason
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(item.ID),
		strings.TrimSpace(item.FamilyID),
		strings.TrimSpace(item.AdminUserID),
		item.AccessTokenHash[:],
		item.RefreshSecretHash[:],
		strings.TrimSpace(item.SourceIP),
		strings.TrimSpace(item.UserAgent),
		formatTime(item.CreatedAt),
		formatTime(item.AccessExpiresAt),
		formatTime(item.ExpiresAt),
		formatTime(item.LastSeenAt),
		formatTime(item.RevokedAt),
		formatTime(item.RotatedAt),
		strings.TrimSpace(item.ReplacedBySessionID),
		item.RevocationReason,
	)
	return err
}

func scanAdminSession(row rowScanner) (adminauth.Session, bool, error) {
	var item adminauth.Session
	var accessHash, refreshHash []byte
	var createdAt, accessExpiresAt, expiresAt, lastSeenAt, revokedAt, rotatedAt string
	if err := row.Scan(
		&item.ID,
		&item.FamilyID,
		&item.AdminUserID,
		&accessHash,
		&refreshHash,
		&item.SourceIP,
		&item.UserAgent,
		&createdAt,
		&accessExpiresAt,
		&expiresAt,
		&lastSeenAt,
		&revokedAt,
		&rotatedAt,
		&item.ReplacedBySessionID,
		&item.RevocationReason,
	); errors.Is(err, sql.ErrNoRows) {
		return adminauth.Session{}, false, nil
	} else if err != nil {
		return adminauth.Session{}, false, fmt.Errorf("scan admin session: %w", err)
	}
	if len(accessHash) != len(item.AccessTokenHash) || len(refreshHash) != len(item.RefreshSecretHash) {
		return adminauth.Session{}, false, errors.New("stored admin session hash has invalid length")
	}
	copy(item.AccessTokenHash[:], accessHash)
	copy(item.RefreshSecretHash[:], refreshHash)
	item.CreatedAt = parseTime(createdAt)
	item.AccessExpiresAt = parseTime(accessExpiresAt)
	item.ExpiresAt = parseTime(expiresAt)
	item.LastSeenAt = parseTime(lastSeenAt)
	item.RevokedAt = parseTime(revokedAt)
	item.RotatedAt = parseTime(rotatedAt)
	return item, true, nil
}
