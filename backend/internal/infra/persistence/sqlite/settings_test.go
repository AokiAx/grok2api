package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/domain/settings"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

func TestSettingsOptimisticLockSnapshotsAndRollback(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if got := repo.SchemaVersion(ctx); got != 8 {
		t.Fatalf("schema=%d", got)
	}
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
	// conflict
	if _, err := repo.PutSettings(ctx, 1, doc, "tester"); !errors.Is(err, repository.ErrSettingsConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	snaps, err := repo.ListSettingsSnapshots(ctx, 10)
	if err != nil || len(snaps) < 2 {
		t.Fatalf("snaps=%v err=%v", snaps, err)
	}
	rolled, err := repo.RollbackSettings(ctx, 2, 1, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Revision != 3 {
		t.Fatalf("rollback rev=%d", rolled.Revision)
	}
	// After rollback-to-1 content, max concurrent should be default 4.
	if rolled.Pool.MaxConcurrent != 4 {
		t.Fatalf("rolled max concurrent=%d", rolled.Pool.MaxConcurrent)
	}
	// Proxy remains not wired.
	if rolled.Proxy.RuntimeStatus != "not_wired" {
		t.Fatalf("proxy status=%q", rolled.Proxy.RuntimeStatus)
	}
	_ = settings.Defaults()
}
