package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

func (r *SQLite) CreateClientKey(ctx context.Context, credential clientkey.Credential) error {
	validated, err := clientkey.NewCredential(credential.Key, credential.Scopes())
	if err != nil {
		return fmt.Errorf("validate client key: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin client key creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertClientKey(ctx, tx, validated.Key); err != nil {
		return fmt.Errorf("create client key %s: %w", validated.Key.ID, err)
	}
	if err := replaceClientKeyScopes(ctx, tx, validated.Key.ID, validated.Scopes(), validated.Key.UpdatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_meta(key, value) VALUES('client_auth_required', '1')
		ON CONFLICT(key) DO UPDATE SET value='1'`); err != nil {
		return fmt.Errorf("enable client authentication marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit client key creation: %w", err)
	}
	return nil
}

func (r *SQLite) GetClientKey(ctx context.Context, id string) (clientkey.Credential, bool, error) {
	return getClientKey(ctx, r.db, `id=?`, strings.TrimSpace(id))
}

func (r *SQLite) FindClientKeyByHash(ctx context.Context, hash [32]byte) (clientkey.Credential, bool, error) {
	if hash == ([32]byte{}) {
		return clientkey.Credential{}, false, errors.New("client key hash is required")
	}
	return getClientKey(ctx, r.db, `key_hash=?`, hash[:])
}

func (r *SQLite) ListClientKeysPage(
	ctx context.Context,
	query repository.ListClientKeysQuery,
) (repository.ListClientKeysResult, error) {
	query = normalizeClientKeyListQuery(query)
	where, args := clientKeyListWhere(query)
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_keys `+where, args...).Scan(&total); err != nil {
		return repository.ListClientKeysResult{}, fmt.Errorf("count client keys: %w", err)
	}
	listArgs := append(append([]any{}, args...), query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := r.db.QueryContext(ctx, clientKeySelect+` `+where+` ORDER BY created_at, id LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return repository.ListClientKeysResult{}, fmt.Errorf("list client keys: %w", err)
	}
	keys := make([]clientkey.ClientKey, 0, query.PageSize)
	for rows.Next() {
		item, err := scanClientKey(rows)
		if err != nil {
			_ = rows.Close()
			return repository.ListClientKeysResult{}, err
		}
		keys = append(keys, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return repository.ListClientKeysResult{}, fmt.Errorf("iterate client keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return repository.ListClientKeysResult{}, fmt.Errorf("close client key rows: %w", err)
	}

	items := make([]clientkey.Credential, 0, len(keys))
	for _, key := range keys {
		scopes, err := loadClientKeyScopes(ctx, r.db, key.ID)
		if err != nil {
			return repository.ListClientKeysResult{}, err
		}
		credential, err := clientkey.NewCredential(key, scopes)
		if err != nil {
			return repository.ListClientKeysResult{}, fmt.Errorf("validate stored client key %s: %w", key.ID, err)
		}
		items = append(items, credential)
	}
	return repository.ListClientKeysResult{
		Items: items, Total: total, Page: query.Page, PageSize: query.PageSize,
	}, nil
}

func (r *SQLite) UpdateClientKeyPolicy(
	ctx context.Context,
	id string,
	update repository.ClientKeyPolicyUpdate,
) error {
	if update.UpdatedAt.IsZero() {
		return errors.New("client key policy update time is required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin client key policy update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored, found, err := getClientKey(ctx, tx, `id=?`, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if !found {
		return sql.ErrNoRows
	}
	candidate := stored.Key
	candidate.Name = update.Name
	candidate.ModelPolicy = update.ModelPolicy
	candidate.RPMLimit = update.RPMLimit
	candidate.MaxConcurrent = update.MaxConcurrent
	candidate.ExpiresAt = update.ExpiresAt
	candidate.UpdatedAt = update.UpdatedAt.UTC()
	validated, err := clientkey.NewCredential(candidate, update.Scopes)
	if err != nil {
		return fmt.Errorf("validate client key policy: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE client_keys SET
		name=?, model_policy=?, rpm_limit=?, max_concurrent=?, expires_at=?, updated_at=?
		WHERE id=?`,
		validated.Key.Name,
		validated.Key.ModelPolicy,
		validated.Key.RPMLimit,
		validated.Key.MaxConcurrent,
		formatTime(validated.Key.ExpiresAt),
		formatTime(validated.Key.UpdatedAt),
		validated.Key.ID,
	)
	if err != nil {
		return fmt.Errorf("update client key policy %s: %w", id, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect client key policy update: %w", err)
	}
	if rowsAffected != 1 {
		return sql.ErrNoRows
	}
	if err := replaceClientKeyScopes(ctx, tx, validated.Key.ID, validated.Scopes(), validated.Key.UpdatedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit client key policy update: %w", err)
	}
	return nil
}

func (r *SQLite) RevokeClientKey(ctx context.Context, id string, at time.Time) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("client key id is required")
	}
	if at.IsZero() {
		return errors.New("client key revocation time is required")
	}
	_, err := r.db.ExecContext(ctx, `UPDATE client_keys SET revoked_at=?, updated_at=?
		WHERE id=? AND revoked_at=''`, formatTime(at), formatTime(at), strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("revoke client key %s: %w", id, err)
	}
	return nil
}

func (r *SQLite) ClientAuthRequired(ctx context.Context) (bool, error) {
	var raw string
	if err := r.db.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key='client_auth_required'`).Scan(&raw); err != nil {
		return false, fmt.Errorf("read client auth marker: %w", err)
	}
	return raw == "1", nil
}

func (r *SQLite) ConsumeClientKeyRPM(
	ctx context.Context,
	keyID string,
	at time.Time,
) (repository.RateLimitDecision, error) {
	if at.IsZero() {
		return repository.RateLimitDecision{}, errors.New("rate limit time is required")
	}
	at = at.UTC()
	windowStart := at.Truncate(time.Minute)
	resetAt := windowStart.Add(time.Minute)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return repository.RateLimitDecision{}, fmt.Errorf("begin client key rate limit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var limit int
	var expiresAt, revokedAt string
	if err := tx.QueryRowContext(ctx, `SELECT rpm_limit, expires_at, revoked_at FROM client_keys WHERE id=?`, strings.TrimSpace(keyID)).Scan(&limit, &expiresAt, &revokedAt); err != nil {
		return repository.RateLimitDecision{}, fmt.Errorf("load client key rate policy: %w", err)
	}
	if revokedAt != "" {
		return repository.RateLimitDecision{}, errors.New("client key is revoked")
	}
	if expiry := parseTime(expiresAt); !expiry.IsZero() && !at.Before(expiry) {
		return repository.RateLimitDecision{}, errors.New("client key is expired")
	}
	if limit == 0 {
		if err := tx.Commit(); err != nil {
			return repository.RateLimitDecision{}, fmt.Errorf("commit unlimited client key rate limit: %w", err)
		}
		return repository.RateLimitDecision{Allowed: true, Limit: 0, Remaining: 0, ResetAt: resetAt}, nil
	}

	var count, allowed int
	err = tx.QueryRowContext(ctx, `INSERT INTO client_key_rate_windows(
		client_key_id, window_start, request_count, last_allowed, updated_at
	) VALUES(?, ?, 1, 1, ?)
	ON CONFLICT(client_key_id) DO UPDATE SET
		request_count=CASE
			WHEN client_key_rate_windows.window_start != excluded.window_start THEN 1
			WHEN client_key_rate_windows.request_count < ? THEN client_key_rate_windows.request_count + 1
			ELSE client_key_rate_windows.request_count
		END,
		last_allowed=CASE
			WHEN client_key_rate_windows.window_start != excluded.window_start THEN 1
			WHEN client_key_rate_windows.request_count < ? THEN 1
			ELSE 0
		END,
		window_start=excluded.window_start,
		updated_at=excluded.updated_at
	RETURNING request_count, last_allowed`,
		strings.TrimSpace(keyID),
		windowStart.Unix(),
		formatTime(at),
		limit,
		limit,
	).Scan(&count, &allowed)
	if err != nil {
		return repository.RateLimitDecision{}, fmt.Errorf("consume client key rate limit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return repository.RateLimitDecision{}, fmt.Errorf("commit client key rate limit: %w", err)
	}
	remaining := limit - count
	if remaining < 0 {
		remaining = 0
	}
	return repository.RateLimitDecision{
		Allowed: allowed == 1, Limit: limit, Remaining: remaining, ResetAt: resetAt,
	}, nil
}

const clientKeySelect = `SELECT
	id, name, origin, key_hash, key_prefix, model_policy, rpm_limit,
	max_concurrent, expires_at, revoked_at, last_used_at, created_at, updated_at
	FROM client_keys`

type contextQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getClientKey(
	ctx context.Context,
	queryer contextQueryer,
	where string,
	arg any,
) (clientkey.Credential, bool, error) {
	key, err := scanClientKey(queryer.QueryRowContext(ctx, clientKeySelect+` WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return clientkey.Credential{}, false, nil
	}
	if err != nil {
		return clientkey.Credential{}, false, err
	}
	scopes, err := loadClientKeyScopes(ctx, queryer, key.ID)
	if err != nil {
		return clientkey.Credential{}, false, err
	}
	credential, err := clientkey.NewCredential(key, scopes)
	if err != nil {
		return clientkey.Credential{}, false, fmt.Errorf("validate stored client key %s: %w", key.ID, err)
	}
	return credential, true, nil
}

func scanClientKey(row rowScanner) (clientkey.ClientKey, error) {
	var item clientkey.ClientKey
	var hash []byte
	var expiresAt, revokedAt, lastUsedAt, createdAt, updatedAt string
	if err := row.Scan(
		&item.ID,
		&item.Name,
		&item.Origin,
		&hash,
		&item.KeyPrefix,
		&item.ModelPolicy,
		&item.RPMLimit,
		&item.MaxConcurrent,
		&expiresAt,
		&revokedAt,
		&lastUsedAt,
		&createdAt,
		&updatedAt,
	); err != nil {
		return clientkey.ClientKey{}, err
	}
	if len(hash) != len(item.KeyHash) {
		return clientkey.ClientKey{}, errors.New("stored client key hash has invalid length")
	}
	copy(item.KeyHash[:], hash)
	item.ExpiresAt = parseTime(expiresAt)
	item.RevokedAt = parseTime(revokedAt)
	item.LastUsedAt = parseTime(lastUsedAt)
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	return item, nil
}

func insertClientKey(ctx context.Context, execer contextExecer, item clientkey.ClientKey) error {
	_, err := execer.ExecContext(ctx, `INSERT INTO client_keys(
		id, name, origin, key_hash, key_prefix, model_policy, rpm_limit,
		max_concurrent, expires_at, revoked_at, last_used_at, created_at, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID,
		item.Name,
		item.Origin,
		item.KeyHash[:],
		item.KeyPrefix,
		item.ModelPolicy,
		item.RPMLimit,
		item.MaxConcurrent,
		formatTime(item.ExpiresAt),
		formatTime(item.RevokedAt),
		formatTime(item.LastUsedAt),
		formatTime(item.CreatedAt),
		formatTime(item.UpdatedAt),
	)
	return err
}

func replaceClientKeyScopes(
	ctx context.Context,
	tx *sql.Tx,
	keyID string,
	scopes []string,
	at time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_key_model_scopes WHERE client_key_id=?`, keyID); err != nil {
		return fmt.Errorf("clear client key scopes %s: %w", keyID, err)
	}
	for _, scope := range scopes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_key_model_scopes(client_key_id, model_id, created_at)
			VALUES(?, ?, ?)`, keyID, scope, formatTime(at)); err != nil {
			return fmt.Errorf("insert client key scope %s/%s: %w", keyID, scope, err)
		}
	}
	return nil
}

func loadClientKeyScopes(ctx context.Context, queryer contextQueryer, keyID string) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT model_id FROM client_key_model_scopes
		WHERE client_key_id=? ORDER BY model_id COLLATE NOCASE`, keyID)
	if err != nil {
		return nil, fmt.Errorf("list client key scopes %s: %w", keyID, err)
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, fmt.Errorf("scan client key scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate client key scopes: %w", err)
	}
	return scopes, nil
}

func normalizeClientKeyListQuery(query repository.ListClientKeysQuery) repository.ListClientKeysQuery {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 50
	}
	if query.PageSize > 200 {
		query.PageSize = 200
	}
	query.Q = strings.TrimSpace(query.Q)
	if query.Origin != clientkey.OriginManaged && query.Origin != clientkey.OriginConfigAPIKey {
		query.Origin = ""
	}
	return query
}

func clientKeyListWhere(query repository.ListClientKeysQuery) (string, []any) {
	var clauses []string
	var args []any
	if query.Origin != "" {
		clauses = append(clauses, "origin=?")
		args = append(args, query.Origin)
	}
	if query.Q != "" {
		like := "%" + escapeLike(query.Q) + "%"
		clauses = append(clauses, `(id LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\' OR key_prefix LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}
