package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/domain/modelreg"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
)

func TestModelRegistrySeedsDefaultsAndSupportsAliasLookup(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "models.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if got := repo.SchemaVersion(ctx); got != 10 {
		t.Fatalf("schema=%d", got)
	}
	items, err := repo.ListModels(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 1 {
		t.Fatalf("expected seeded models, got %d", len(items))
	}
	item := items[0]
	item.Aliases = []string{"grok-latest"}
	item.Enabled = true
	item.Source = "managed"
	if err := repo.UpsertModel(ctx, item); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetModel(ctx, "grok-latest")
	if err != nil || !found {
		t.Fatalf("alias lookup found=%v err=%v", found, err)
	}
	if got.ID != item.ID {
		t.Fatalf("alias resolved to %q want %q", got.ID, item.ID)
	}
	// Disable and ensure excluded from enabled list.
	got.Enabled = false
	if err := repo.UpsertModel(ctx, got); err != nil {
		t.Fatal(err)
	}
	enabled, err := repo.ListModels(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range enabled {
		if m.ID == got.ID {
			t.Fatalf("disabled model still listed: %s", m.ID)
		}
	}
	catalog := sqlite.CatalogFromRegistry([]modelreg.Model{got})
	if catalog == nil {
		t.Fatal("catalog nil")
	}
}
