package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/modelreg"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

var _ repository.ModelRegistryRepository = (*SQLite)(nil)

func (r *SQLite) ensureModelRegistrySchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS models (
			id TEXT PRIMARY KEY COLLATE NOCASE,
			upstream_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			api_backend TEXT NOT NULL DEFAULT 'responses',
			context_window INTEGER NOT NULL DEFAULT 0,
			supports_reasoning_effort INTEGER NOT NULL DEFAULT 0 CHECK(supports_reasoning_effort IN (0,1)),
			reasoning_efforts_json TEXT NOT NULL DEFAULT '[]',
			supports_backend_search INTEGER NOT NULL DEFAULT 0 CHECK(supports_backend_search IN (0,1)),
			owned_by TEXT NOT NULL DEFAULT 'xai',
			enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
			aliases_json TEXT NOT NULL DEFAULT '[]',
			source TEXT NOT NULL DEFAULT 'managed' CHECK(source IN ('seed','managed')),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_models_enabled ON models(enabled, id)`,
	}
	for _, statement := range statements {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure model registry schema: %w", err)
		}
	}
	// Seed defaults once; never overwrite managed edits.
	seeds := make([]modelreg.Model, 0, len(upstream.DefaultCatalog()))
	now := time.Now().UTC()
	for _, item := range upstream.DefaultCatalog() {
		seeds = append(seeds, modelreg.Model{
			ID:                      item.ID,
			UpstreamID:              item.ID,
			Name:                    item.Name,
			APIBackend:              item.APIBackend,
			ContextWindow:           item.ContextWindow,
			SupportsReasoningEffort: item.SupportsReasoningEffort,
			ReasoningEfforts:        append([]string(nil), item.ReasoningEfforts...),
			SupportsBackendSearch:   item.SupportsBackendSearch,
			OwnedBy:                 item.OwnedBy,
			Enabled:                 true,
			Source:                  "seed",
			CreatedAt:               now,
			UpdatedAt:               now,
		})
	}
	if _, err := r.SeedModels(ctx, seeds); err != nil {
		return err
	}
	return nil
}

func (r *SQLite) SeedModels(ctx context.Context, models []modelreg.Model) (int, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("sqlite repository is not open")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	inserted := 0
	for _, item := range models {
		if err := item.Normalize(); err != nil {
			return 0, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM models WHERE id=? COLLATE NOCASE`, item.ID).Scan(&exists); err != nil {
			return 0, err
		}
		if exists > 0 {
			continue
		}
		if err := upsertModelTx(ctx, tx, item); err != nil {
			return 0, err
		}
		inserted++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

func (r *SQLite) UpsertModel(ctx context.Context, item modelreg.Model) error {
	if err := item.Normalize(); err != nil {
		return err
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	item.UpdatedAt = time.Now().UTC()
	return upsertModelTx(ctx, r.db, item)
}

type execContext interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func upsertModelTx(ctx context.Context, exec execContext, item modelreg.Model) error {
	efforts, err := json.Marshal(item.ReasoningEfforts)
	if err != nil {
		return err
	}
	aliases, err := json.Marshal(item.Aliases)
	if err != nil {
		return err
	}
	enabled := 0
	if item.Enabled {
		enabled = 1
	}
	reasoning := 0
	if item.SupportsReasoningEffort {
		reasoning = 1
	}
	search := 0
	if item.SupportsBackendSearch {
		search = 1
	}
	_, err = exec.ExecContext(
		ctx,
		`INSERT INTO models (
			id, upstream_id, name, api_backend, context_window, supports_reasoning_effort,
			reasoning_efforts_json, supports_backend_search, owned_by, enabled, aliases_json,
			source, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			upstream_id=excluded.upstream_id,
			name=excluded.name,
			api_backend=excluded.api_backend,
			context_window=excluded.context_window,
			supports_reasoning_effort=excluded.supports_reasoning_effort,
			reasoning_efforts_json=excluded.reasoning_efforts_json,
			supports_backend_search=excluded.supports_backend_search,
			owned_by=excluded.owned_by,
			enabled=excluded.enabled,
			aliases_json=excluded.aliases_json,
			source=excluded.source,
			updated_at=excluded.updated_at`,
		item.ID,
		item.UpstreamID,
		item.Name,
		item.APIBackend,
		item.ContextWindow,
		reasoning,
		string(efforts),
		search,
		item.OwnedBy,
		enabled,
		string(aliases),
		item.Source,
		item.CreatedAt.UTC().Format(time.RFC3339Nano),
		item.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert model: %w", err)
	}
	return nil
}

func (r *SQLite) ListModels(ctx context.Context, includeDisabled bool) ([]modelreg.Model, error) {
	query := `SELECT id, upstream_id, name, api_backend, context_window, supports_reasoning_effort,
		reasoning_efforts_json, supports_backend_search, owned_by, enabled, aliases_json, source, created_at, updated_at
		FROM models`
	if !includeDisabled {
		query += ` WHERE enabled=1`
	}
	query += ` ORDER BY id COLLATE NOCASE`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modelreg.Model
	for rows.Next() {
		item, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *SQLite) GetModel(ctx context.Context, id string) (modelreg.Model, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return modelreg.Model{}, false, nil
	}
	row := r.db.QueryRowContext(
		ctx,
		`SELECT id, upstream_id, name, api_backend, context_window, supports_reasoning_effort,
			reasoning_efforts_json, supports_backend_search, owned_by, enabled, aliases_json, source, created_at, updated_at
		 FROM models
		 WHERE id=? COLLATE NOCASE
		 LIMIT 1`,
		id,
	)
	item, err := scanModel(row)
	if err == nil {
		return item, true, nil
	}
	if err != sql.ErrNoRows {
		return modelreg.Model{}, false, err
	}
	// Alias lookup without depending on json_each availability.
	items, listErr := r.ListModels(ctx, true)
	if listErr != nil {
		return modelreg.Model{}, false, listErr
	}
	needle := strings.ToLower(id)
	for _, candidate := range items {
		if strings.ToLower(candidate.ID) == needle {
			return candidate, true, nil
		}
		for _, alias := range candidate.Aliases {
			if strings.ToLower(strings.TrimSpace(alias)) == needle {
				return candidate, true, nil
			}
		}
	}
	return modelreg.Model{}, false, nil
}

type modelScanner interface {
	Scan(dest ...any) error
}

func scanModel(row modelScanner) (modelreg.Model, error) {
	var (
		item                       modelreg.Model
		reasoning, search, enabled int
		effortsJSON, aliasesJSON   string
		createdAt, updatedAt       string
	)
	if err := row.Scan(
		&item.ID,
		&item.UpstreamID,
		&item.Name,
		&item.APIBackend,
		&item.ContextWindow,
		&reasoning,
		&effortsJSON,
		&search,
		&item.OwnedBy,
		&enabled,
		&aliasesJSON,
		&item.Source,
		&createdAt,
		&updatedAt,
	); err != nil {
		return modelreg.Model{}, err
	}
	item.SupportsReasoningEffort = reasoning == 1
	item.SupportsBackendSearch = search == 1
	item.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(effortsJSON), &item.ReasoningEfforts)
	_ = json.Unmarshal([]byte(aliasesJSON), &item.Aliases)
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return item, nil
}

// CatalogFromRegistry builds an upstream.Catalog facade from persisted models.
func CatalogFromRegistry(models []modelreg.Model) *upstream.Catalog {
	items := make([]upstream.ModelInfo, 0, len(models))
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		info := upstream.ModelInfo{
			ID:                      model.ID,
			UpstreamID:              model.ResolveUpstream(),
			Name:                    model.Name,
			APIBackend:              model.APIBackend,
			ContextWindow:           model.ContextWindow,
			SupportsReasoningEffort: model.SupportsReasoningEffort,
			ReasoningEfforts:        append([]string(nil), model.ReasoningEfforts...),
			SupportsBackendSearch:   model.SupportsBackendSearch,
			OwnedBy:                 model.OwnedBy,
		}
		items = append(items, info)
		// Alias entries map public alias -> same metadata id for lookup, but List
		// still uses primary ids. Upstream.Catalog keys by id; register aliases too.
		for _, alias := range model.Aliases {
			aliasInfo := info
			aliasInfo.ID = alias
			// Keep UpstreamID pointing at the provider id, not the public alias.
			items = append(items, aliasInfo)
		}
	}
	if len(items) == 0 {
		return upstream.NewDefaultCatalog()
	}
	return upstream.NewCatalog(items)
}
