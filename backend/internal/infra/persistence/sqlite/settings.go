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
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS app_settings (
		id INTEGER PRIMARY KEY CHECK(id = 1),
		revision INTEGER NOT NULL DEFAULT 1,
		document_json TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		updated_by TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return fmt.Errorf("ensure settings schema: %w", err)
	}
	// Drop legacy snapshot history; settings now keep only the current document.
	if _, err := r.db.ExecContext(ctx, `DROP TABLE IF EXISTS app_settings_snapshots`); err != nil {
		return fmt.Errorf("drop settings snapshots: %w", err)
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
	if err := tx.Commit(); err != nil {
		return settings.Document{}, err
	}
	return doc, nil
}
