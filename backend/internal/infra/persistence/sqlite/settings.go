package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/settings"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

var _ repository.SettingsRepository = (*SQLite)(nil)

func (r *SQLite) ensureSettingsSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS app_settings (
			id INTEGER PRIMARY KEY CHECK(id = 1),
			revision INTEGER NOT NULL DEFAULT 1,
			document_json TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			updated_by TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS app_settings_snapshots (
			revision INTEGER PRIMARY KEY,
			created_at TEXT NOT NULL,
			created_by TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			document_json TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure settings schema: %w", err)
		}
	}
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM app_settings WHERE id=1`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		doc := settings.Defaults()
		raw, err := doc.Marshal()
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := r.db.ExecContext(
			ctx,
			`INSERT INTO app_settings(id, revision, document_json, updated_at, updated_by) VALUES (1, ?, ?, ?, '')`,
			doc.Revision,
			string(raw),
			now,
		); err != nil {
			return fmt.Errorf("seed settings: %w", err)
		}
		if _, err := r.db.ExecContext(
			ctx,
			`INSERT INTO app_settings_snapshots(revision, created_at, created_by, reason, document_json)
			 VALUES (?, ?, '', 'seed', ?)`,
			doc.Revision,
			now,
			string(raw),
		); err != nil {
			return fmt.Errorf("seed settings snapshot: %w", err)
		}
	}
	return nil
}

func (r *SQLite) GetSettings(ctx context.Context) (settings.Document, error) {
	var revision int64
	var raw, updatedAt, updatedBy string
	err := r.db.QueryRowContext(
		ctx,
		`SELECT revision, document_json, updated_at, updated_by FROM app_settings WHERE id=1`,
	).Scan(&revision, &raw, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return settings.Document{}, repository.ErrSettingsNotFound
	}
	if err != nil {
		return settings.Document{}, err
	}
	doc, err := settings.Unmarshal([]byte(raw))
	if err != nil {
		return settings.Document{}, err
	}
	doc.Revision = revision
	doc.UpdatedBy = updatedBy
	if ts, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
		doc.UpdatedAt = ts
	}
	return doc, nil
}

func (r *SQLite) PutSettings(ctx context.Context, expectedRevision int64, doc settings.Document, updatedBy string) (settings.Document, error) {
	if err := doc.Normalize(); err != nil {
		return settings.Document{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return settings.Document{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM app_settings WHERE id=1`).Scan(&currentRevision); err != nil {
		return settings.Document{}, err
	}
	if currentRevision != expectedRevision {
		return settings.Document{}, repository.ErrSettingsConflict
	}
	next := currentRevision + 1
	doc.Revision = next
	doc.UpdatedAt = time.Now().UTC()
	doc.UpdatedBy = updatedBy
	raw, err := json.Marshal(doc)
	if err != nil {
		return settings.Document{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE app_settings SET revision=?, document_json=?, updated_at=?, updated_by=? WHERE id=1`,
		next,
		string(raw),
		doc.UpdatedAt.Format(time.RFC3339Nano),
		updatedBy,
	); err != nil {
		return settings.Document{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO app_settings_snapshots(revision, created_at, created_by, reason, document_json)
		 VALUES (?, ?, ?, 'update', ?)`,
		next,
		doc.UpdatedAt.Format(time.RFC3339Nano),
		updatedBy,
		string(raw),
	); err != nil {
		return settings.Document{}, err
	}
	if err := tx.Commit(); err != nil {
		return settings.Document{}, err
	}
	return doc, nil
}

func (r *SQLite) ListSettingsSnapshots(ctx context.Context, limit int) ([]settings.Snapshot, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT revision, created_at, created_by, reason, document_json
		 FROM app_settings_snapshots
		 ORDER BY revision DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []settings.Snapshot
	for rows.Next() {
		var snap settings.Snapshot
		var createdAt, raw string
		if err := rows.Scan(&snap.Revision, &createdAt, &snap.CreatedBy, &snap.Reason, &raw); err != nil {
			return nil, err
		}
		snap.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		doc, err := settings.Unmarshal([]byte(raw))
		if err != nil {
			return nil, err
		}
		doc.Revision = snap.Revision
		snap.Document = doc
		out = append(out, snap)
	}
	return out, rows.Err()
}

func (r *SQLite) GetSettingsSnapshot(ctx context.Context, revision int64) (settings.Snapshot, bool, error) {
	var snap settings.Snapshot
	var createdAt, raw string
	err := r.db.QueryRowContext(
		ctx,
		`SELECT revision, created_at, created_by, reason, document_json
		 FROM app_settings_snapshots WHERE revision=?`,
		revision,
	).Scan(&snap.Revision, &createdAt, &snap.CreatedBy, &snap.Reason, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings.Snapshot{}, false, nil
	}
	if err != nil {
		return settings.Snapshot{}, false, err
	}
	snap.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	doc, err := settings.Unmarshal([]byte(raw))
	if err != nil {
		return settings.Snapshot{}, false, err
	}
	doc.Revision = snap.Revision
	snap.Document = doc
	return snap, true, nil
}

func (r *SQLite) RollbackSettings(ctx context.Context, expectedRevision, targetRevision int64, updatedBy string) (settings.Document, error) {
	snap, found, err := r.GetSettingsSnapshot(ctx, targetRevision)
	if err != nil {
		return settings.Document{}, err
	}
	if !found {
		return settings.Document{}, repository.ErrSettingsSnapshotGone
	}
	doc := snap.Document
	doc.UpdatedBy = updatedBy
	// PutSettings creates a new revision from the snapshot content.
	// Annotate reason via temporary reason field in Put path: reuse Put then patch snapshot reason.
	put, err := r.PutSettings(ctx, expectedRevision, doc, updatedBy)
	if err != nil {
		return settings.Document{}, err
	}
	// Rewrite latest snapshot reason to rollback.
	_, _ = r.db.ExecContext(
		ctx,
		`UPDATE app_settings_snapshots SET reason=? WHERE revision=?`,
		fmt.Sprintf("rollback_to_%d", targetRevision),
		put.Revision,
	)
	return put, nil
}
