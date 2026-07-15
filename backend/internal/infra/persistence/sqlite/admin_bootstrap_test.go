package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func newBootstrapAdmin(t *testing.T, id, username string) adminauth.AdminUser {
	t.Helper()
	credential, err := security.HashAdminPassword("long-enough-password", bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user, err := adminauth.NewAdminUser(id, username, credential, time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("new admin: %v", err)
	}
	return user
}

func TestSQLiteBootstrapAdminCreatesMarkerAndDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "admin-bootstrap.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()

	first := newBootstrapAdmin(t, "admin-1", "admin")
	status, err := repo.BootstrapAdmin(ctx, first)
	if err != nil || status != repository.BootstrapCreated {
		t.Fatalf("first status=%q err=%v", status, err)
	}

	raw, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var marker string
	if err := raw.QueryRow(`SELECT value FROM app_meta WHERE key='admin_bootstrap_v1'`).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("admin count=%d err=%v", count, err)
	}

	second := newBootstrapAdmin(t, "admin-2", "replacement")
	status, err = repo.BootstrapAdmin(ctx, second)
	if err != nil || status != repository.BootstrapAlreadyCompleted {
		t.Fatalf("second status=%q err=%v", status, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("admin count after second=%d err=%v", count, err)
	}
}

func TestSQLiteBootstrapAdminExistingWithoutMarkerDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "admin-existing.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()
	first := newBootstrapAdmin(t, "admin-1", "admin")
	if err := repo.CreateAdminUser(ctx, first); err != nil {
		t.Fatalf("create existing admin: %v", err)
	}
	second := newBootstrapAdmin(t, "admin-2", "replacement")
	status, err := repo.BootstrapAdmin(ctx, second)
	if err != nil || status != repository.BootstrapExisting {
		t.Fatalf("status=%q err=%v", status, err)
	}
	stored, found, err := repo.GetAdminUserByUsername(ctx, "admin")
	if err != nil || !found || stored.ID != first.ID {
		t.Fatalf("stored=%+v found=%v err=%v", stored, found, err)
	}
}

func TestSQLiteBootstrapAdminRollsBackUserAndMarkerOnInsertFailure(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "admin-rollback.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()
	raw, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER reject_admin_bootstrap BEFORE INSERT ON admin_users BEGIN SELECT RAISE(ABORT, 'fixture rejection'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	_ = raw.Close()

	status, err := repo.BootstrapAdmin(ctx, newBootstrapAdmin(t, "admin-1", "admin"))
	if err == nil || status != "" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	raw, err = sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("admin count=%d err=%v", count, err)
	}
	var marker string
	if err := raw.QueryRow(`SELECT value FROM app_meta WHERE key='admin_bootstrap_v1'`).Scan(&marker); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("marker=%q err=%v, want no marker", marker, err)
	}
}

func TestSQLiteBootstrapAdminConcurrentCallsCreateOneAdministrator(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "admin-concurrent.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()

	users := []adminauth.AdminUser{
		newBootstrapAdmin(t, "admin-1", "admin"),
		newBootstrapAdmin(t, "admin-2", "replacement"),
	}
	statuses := make([]repository.BootstrapStatus, len(users))
	errorsByCall := make([]error, len(users))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range users {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			statuses[index], errorsByCall[index] = repo.BootstrapAdmin(ctx, users[index])
		}(index)
	}
	close(start)
	wait.Wait()

	created := 0
	closed := 0
	for index, status := range statuses {
		if errorsByCall[index] != nil {
			t.Fatalf("call %d status=%q err=%v", index, status, errorsByCall[index])
		}
		switch status {
		case repository.BootstrapCreated:
			created++
		case repository.BootstrapAlreadyCompleted, repository.BootstrapExisting:
			closed++
		default:
			t.Fatalf("call %d returned unsupported status %q", index, status)
		}
	}
	if created != 1 || closed != 1 {
		t.Fatalf("statuses=%v", statuses)
	}
	if count, err := repo.CountAdminUsers(ctx); err != nil || count != 1 {
		t.Fatalf("admin count=%d err=%v", count, err)
	}
}
