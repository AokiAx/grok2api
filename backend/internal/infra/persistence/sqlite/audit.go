package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/audit"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

var _ repository.AuditRepository = (*SQLite)(nil)

func (r *SQLite) ensureRequestAuditSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS request_audits (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			method TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			operation TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			client_key_id TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL DEFAULT 0,
			success INTEGER NOT NULL DEFAULT 0 CHECK(success IN (0,1)),
			error_type TEXT NOT NULL DEFAULT '',
			error_code TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			cached_input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			stream INTEGER NOT NULL DEFAULT 0 CHECK(stream IN (0,1))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_audits_started ON request_audits(started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_audits_model ON request_audits(model, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_audits_account ON request_audits(account_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_audits_success ON request_audits(success, started_at)`,
		`CREATE TABLE IF NOT EXISTS request_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL REFERENCES request_audits(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0,
			success INTEGER NOT NULL DEFAULT 0 CHECK(success IN (0,1)),
			error_type TEXT NOT NULL DEFAULT '',
			error_code TEXT NOT NULL DEFAULT '',
			rotated INTEGER NOT NULL DEFAULT 0 CHECK(rotated IN (0,1)),
			UNIQUE(request_id, ordinal)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_attempts_request ON request_attempts(request_id)`,
	}
	for _, statement := range statements {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure request audit schema: %w", err)
		}
	}
	if err := r.ensureRequestAuditColumns(ctx); err != nil {
		return err
	}
	return nil
}

func (r *SQLite) ensureRequestAuditColumns(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `PRAGMA table_info(request_audits)`)
	if err != nil {
		return fmt.Errorf("inspect request_audits columns: %w", err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan request_audits column: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close request_audits columns: %w", err)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !columns["cached_input_tokens"] {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE request_audits ADD COLUMN cached_input_tokens INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add cached_input_tokens column: %w", err)
		}
	}
	return nil
}

func (r *SQLite) RecordRequestAudit(ctx context.Context, item audit.Request, attempts []audit.Attempt) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("sqlite repository is not open")
	}
	if strings.TrimSpace(item.ID) == "" {
		return fmt.Errorf("request audit id is required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	success := 0
	if item.Success {
		success = 1
	}
	stream := 0
	if item.Stream {
		stream = 1
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO request_audits (
			id, request_id, started_at, finished_at, duration_ms, method, path, operation,
			model, client_key_id, account_id, status_code, success, error_type, error_code,
			input_tokens, cached_input_tokens, output_tokens, total_tokens, attempt_count, stream
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			request_id=excluded.request_id,
			started_at=excluded.started_at,
			finished_at=excluded.finished_at,
			duration_ms=excluded.duration_ms,
			method=excluded.method,
			path=excluded.path,
			operation=excluded.operation,
			model=excluded.model,
			client_key_id=excluded.client_key_id,
			account_id=excluded.account_id,
			status_code=excluded.status_code,
			success=excluded.success,
			error_type=excluded.error_type,
			error_code=excluded.error_code,
			input_tokens=excluded.input_tokens,
			cached_input_tokens=excluded.cached_input_tokens,
			output_tokens=excluded.output_tokens,
			total_tokens=excluded.total_tokens,
			attempt_count=excluded.attempt_count,
			stream=excluded.stream`,
		item.ID,
		item.RequestID,
		item.StartedAt.UTC().Format(time.RFC3339Nano),
		item.FinishedAt.UTC().Format(time.RFC3339Nano),
		item.DurationMS,
		item.Method,
		item.Path,
		item.Operation,
		item.Model,
		item.ClientKeyID,
		item.AccountID,
		item.StatusCode,
		success,
		item.ErrorType,
		item.ErrorCode,
		item.InputTokens,
		item.CachedInputTokens,
		item.OutputTokens,
		item.TotalTokens,
		item.AttemptCount,
		stream,
	); err != nil {
		return fmt.Errorf("insert request audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM request_attempts WHERE request_id=?`, item.ID); err != nil {
		return fmt.Errorf("clear request attempts: %w", err)
	}
	for _, attempt := range attempts {
		attemptSuccess := 0
		if attempt.Success {
			attemptSuccess = 1
		}
		rotated := 0
		if attempt.Rotated {
			rotated = 1
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO request_attempts (
				request_id, ordinal, account_id, started_at, finished_at, duration_ms,
				status_code, success, error_type, error_code, rotated
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ID,
			attempt.Ordinal,
			attempt.AccountID,
			attempt.StartedAt.UTC().Format(time.RFC3339Nano),
			attempt.FinishedAt.UTC().Format(time.RFC3339Nano),
			attempt.DurationMS,
			attempt.StatusCode,
			attemptSuccess,
			attempt.ErrorType,
			attempt.ErrorCode,
			rotated,
		); err != nil {
			return fmt.Errorf("insert request attempt: %w", err)
		}
	}
	return tx.Commit()
}

func (r *SQLite) AuditUsageSummary(ctx context.Context, from, to time.Time) (audit.UsageSummary, error) {
	var out audit.UsageSummary
	err := r.db.QueryRowContext(
		ctx,
		`SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN success=1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(cached_input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0)
		FROM request_audits
		WHERE started_at >= ? AND started_at < ?`,
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
	).Scan(
		&out.Requests,
		&out.SuccessfulRequests,
		&out.FailedRequests,
		&out.InputTokens,
		&out.CachedInputTokens,
		&out.OutputTokens,
		&out.TotalTokens,
	)
	if err != nil {
		return audit.UsageSummary{}, err
	}
	if out.Requests > 0 {
		out.SuccessRate = float64(out.SuccessfulRequests) * 100 / float64(out.Requests)
	}
	// Approximate p95 via ordered sample of durations.
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT duration_ms FROM request_audits
		 WHERE started_at >= ? AND started_at < ?
		 ORDER BY duration_ms`,
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var durations []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return out, err
		}
		durations = append(durations, d)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if n := len(durations); n > 0 {
		idx := int(float64(n-1) * 0.95)
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		out.P95DurationMS = durations[idx]
	}
	return out, nil
}

func (r *SQLite) AuditSeries(ctx context.Context, from, to time.Time, bucket time.Duration) ([]audit.SeriesPoint, error) {
	if bucket <= 0 {
		bucket = time.Hour
	}
	step := int64(bucket.Seconds())
	if step < 60 {
		step = 60
	}
	fromUTC := from.UTC()
	toUTC := to.UTC()
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT
			(CAST(strftime('%s', replace(substr(started_at,1,19),'T',' ')) AS INTEGER) / ?) * ? AS bucket,
			COUNT(*),
			COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(total_tokens), 0)
		FROM request_audits
		WHERE started_at >= ? AND started_at < ?
		GROUP BY bucket
		ORDER BY bucket`,
		step, step, fromUTC.Format(time.RFC3339Nano), toUTC.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byBucket := make(map[int64]audit.SeriesPoint)
	for rows.Next() {
		var bucketUnix, requests, failures, tokens int64
		if err := rows.Scan(&bucketUnix, &requests, &failures, &tokens); err != nil {
			return nil, err
		}
		byBucket[bucketUnix] = audit.SeriesPoint{
			BucketStart: time.Unix(bucketUnix, 0).UTC(),
			BucketEnd:   time.Unix(bucketUnix+step, 0).UTC(),
			Requests:    requests,
			Failures:    failures,
			Tokens:      tokens,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	modelRows, err := r.db.QueryContext(
		ctx,
		`SELECT
			(CAST(strftime('%s', replace(substr(started_at,1,19),'T',' ')) AS INTEGER) / ?) * ? AS bucket,
			CASE WHEN TRIM(model)='' THEN '(unknown)' ELSE model END AS name,
			COALESCE(SUM(total_tokens), 0) AS tokens
		FROM request_audits
		WHERE started_at >= ? AND started_at < ?
		GROUP BY bucket, name
		ORDER BY bucket, tokens DESC, name`,
		step, step, fromUTC.Format(time.RFC3339Nano), toUTC.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer modelRows.Close()
	modelsByBucket := make(map[int64][]audit.SeriesModelUsage)
	for modelRows.Next() {
		var bucketUnix, tokens int64
		var name string
		if err := modelRows.Scan(&bucketUnix, &name, &tokens); err != nil {
			return nil, err
		}
		if len(modelsByBucket[bucketUnix]) >= 8 {
			continue
		}
		modelsByBucket[bucketUnix] = append(modelsByBucket[bucketUnix], audit.SeriesModelUsage{
			Model:  name,
			Tokens: tokens,
		})
	}
	if err := modelRows.Err(); err != nil {
		return nil, err
	}

	startUnix := fromUTC.Unix()
	endUnix := toUTC.Unix()
	if startUnix%step != 0 {
		startUnix = startUnix - (startUnix % step)
	}
	out := make([]audit.SeriesPoint, 0, int((endUnix-startUnix)/step)+1)
	for ts := startUnix; ts < endUnix; ts += step {
		point, ok := byBucket[ts]
		if !ok {
			point = audit.SeriesPoint{
				BucketStart: time.Unix(ts, 0).UTC(),
				BucketEnd:   time.Unix(ts+step, 0).UTC(),
			}
		} else if point.BucketEnd.IsZero() {
			point.BucketEnd = time.Unix(ts+step, 0).UTC()
		}
		point.Models = modelsByBucket[ts]
		out = append(out, point)
	}
	return out, nil
}

func (r *SQLite) AuditTopModels(ctx context.Context, from, to time.Time, limit int) ([]audit.NamedCount, error) {
	return r.auditTop(ctx, from, to, limit, "model")
}

func (r *SQLite) AuditTopAccounts(ctx context.Context, from, to time.Time, limit int) ([]audit.NamedCount, error) {
	return r.auditTop(ctx, from, to, limit, "account_id")
}

func (r *SQLite) auditTop(ctx context.Context, from, to time.Time, limit int, column string) ([]audit.NamedCount, error) {
	if limit <= 0 {
		limit = 10
	}
	if column != "model" && column != "account_id" {
		return nil, fmt.Errorf("unsupported top column")
	}
	query := fmt.Sprintf(
		`SELECT CASE WHEN TRIM(%s)='' THEN '(unknown)' ELSE %s END AS name,
		        COUNT(*) AS c,
		        COALESCE(SUM(total_tokens), 0) AS tokens
		 FROM request_audits
		 WHERE started_at >= ? AND started_at < ?
		 GROUP BY name
		 ORDER BY c DESC, tokens DESC, name ASC
		 LIMIT ?`, column, column,
	)
	rows, err := r.db.QueryContext(ctx, query, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []audit.NamedCount
	for rows.Next() {
		var item audit.NamedCount
		if err := rows.Scan(&item.Name, &item.Count, &item.Tokens); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *SQLite) AuditRecentFailures(ctx context.Context, from, to time.Time, limit int) ([]audit.RecentFailure, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT request_id, started_at, model, account_id, status_code, error_type, error_code, path, duration_ms
		 FROM request_audits
		 WHERE success=0 AND started_at >= ? AND started_at < ?
		 ORDER BY started_at DESC
		 LIMIT ?`,
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []audit.RecentFailure
	for rows.Next() {
		var item audit.RecentFailure
		var started string
		if err := rows.Scan(
			&item.RequestID,
			&started,
			&item.Model,
			&item.AccountID,
			&item.StatusCode,
			&item.ErrorType,
			&item.ErrorCode,
			&item.Path,
			&item.DurationMS,
		); err != nil {
			return nil, err
		}
		item.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *SQLite) PruneRequestAudits(ctx context.Context, olderThan time.Time) (int64, error) {
	result, err := r.db.ExecContext(
		ctx,
		`DELETE FROM request_audits WHERE started_at < ?`,
		olderThan.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CountRequestAudits is a test helper.
func (r *SQLite) CountRequestAudits(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_audits`).Scan(&n)
	return n, err
}

// ensure no unused import when sql only used in types elsewhere
var _ = sql.ErrNoRows
