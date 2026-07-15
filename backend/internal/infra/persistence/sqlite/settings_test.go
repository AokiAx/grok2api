package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

func TestSettingsOptimisticLock(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	doc, err := repo.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Revision != 1 || doc.Audit.RetentionDays != 30 {
		t.Fatalf("seed=%+v", doc)
	}
	doc.Pool.MaxConcurrent = 8
	doc.Audit.RetentionDays = 14
	updated, err := repo.PutSettings(ctx, 1, doc, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Pool.MaxConcurrent != 8 {
		t.Fatalf("updated=%+v", updated)
	}
	if _, err := repo.PutSettings(ctx, 1, doc, "tester"); !errors.Is(err, repository.ErrSettingsConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	current, err := repo.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 || current.Pool.MaxConcurrent != 8 {
		t.Fatalf("current=%+v", current)
	}
	if current.Proxy.RuntimeStatus != "disabled" {
		t.Fatalf("proxy status=%q", current.Proxy.RuntimeStatus)
	}
}
