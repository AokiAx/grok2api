package admin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
)

type memoryRepository struct {
	accounts map[string]account.Account
	saves    int
	deletes  int
}

type memorySink struct {
	items   []account.Account
	deleted []string
}

func (s *memorySink) Upsert(item account.Account) {
	s.items = append(s.items, item)
}

func (s *memorySink) Delete(id string) {
	s.deleted = append(s.deleted, id)
}

func (r *memoryRepository) ListAccounts(context.Context) ([]account.Account, error) {
	result := make([]account.Account, 0, len(r.accounts))
	for _, item := range r.accounts {
		result = append(result, item)
	}
	return result, nil
}

func (r *memoryRepository) SaveAccount(_ context.Context, item account.Account) error {
	if r.accounts == nil {
		r.accounts = map[string]account.Account{}
	}
	r.accounts[item.ID] = item
	r.saves++
	return nil
}

func (r *memoryRepository) DeleteAccount(_ context.Context, id string) error {
	delete(r.accounts, id)
	r.deletes++
	return nil
}

type validator struct {
	reason account.UnavailableReason
	code   string
	err    error
}

func (v validator) Validate(context.Context, account.Account) (account.UnavailableReason, string, error) {
	return v.reason, v.code, v.err
}

func TestImportValidAccountEntersReadyPool(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})

	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			Email:        "User@Example.com",
		}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Added != 1 || repository.saves != 1 {
		t.Fatalf("result = %#v saves=%d", result, repository.saves)
	}
	for _, item := range repository.accounts {
		if item.Pool != account.PoolReady {
			t.Fatalf("pool = %q; want ready", item.Pool)
		}
		if item.Email != "user@example.com" {
			t.Fatalf("email = %q", item.Email)
		}
	}
}

func TestImportAuthenticationFailureEntersUnavailablePool(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{
		reason: account.ReasonAuth,
		code:   "invalid-token",
	})

	_, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{AccessToken: "bad-token"}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, item := range repository.accounts {
		if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonAuth {
			t.Fatalf("account = %#v", item)
		}
	}
}

func TestImportPreviewDoesNotPersist(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})

	result, err := service.Import(context.Background(), admin.ImportRequest{
		DryRun:   true,
		Accounts: []admin.ImportAccount{{AccessToken: "preview-token"}},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if result.Added != 1 || repository.saves != 0 {
		t.Fatalf("result = %#v saves=%d", result, repository.saves)
	}
}

func TestImportValidatorInfrastructureErrorStopsImport(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{err: errors.New("network down")})

	_, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{AccessToken: "token"}},
	})
	if err == nil {
		t.Fatal("expected validator error")
	}
}

func TestImportUpdatesExistingEmailAndAddsToScheduler(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"existing": {
			ID:           "existing",
			AccessToken:  "old-token",
			RefreshToken: "old-refresh",
			Email:        "user@example.com",
			Pool:         account.PoolUnavailable,
		},
	}}
	sink := &memorySink{}
	service := admin.NewService(repository, validator{}, admin.WithSink(sink))

	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{
			AccessToken: "new-token",
			Email:       "USER@example.com",
		}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Updated != 1 || result.Added != 0 {
		t.Fatalf("result = %#v", result)
	}
	if repository.accounts["existing"].AccessToken != "new-token" {
		t.Fatalf("account = %#v", repository.accounts["existing"])
	}
	if len(sink.items) != 1 || sink.items[0].Pool != account.PoolReady {
		t.Fatalf("sink = %#v", sink.items)
	}
}

func TestImportRejectsMissingAccessToken(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})
	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{Email: "user@example.com"}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Invalid != 1 || repository.saves != 0 {
		t.Fatalf("result = %#v saves=%d", result, repository.saves)
	}
}

func TestDeleteRemovesAccountFromRepositoryAndScheduler(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"account-1": {ID: "account-1", Pool: account.PoolReady},
	}}
	sink := &memorySink{}
	service := admin.NewService(repository, validator{}, admin.WithSink(sink))

	if err := service.Delete(context.Background(), "account-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if repository.deletes != 1 || len(repository.accounts) != 0 {
		t.Fatalf("repository = %#v deletes=%d", repository.accounts, repository.deletes)
	}
	if len(sink.deleted) != 1 || sink.deleted[0] != "account-1" {
		t.Fatalf("sink deleted = %#v", sink.deleted)
	}
}

func TestRecoverValidatesUnavailableAccountBeforeReturningReady(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"account-1": {
			ID:                "account-1",
			AccessToken:       "token",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			RetryAt:           time.Now().Add(time.Hour),
		},
	}}
	sink := &memorySink{}
	service := admin.NewService(repository, validator{}, admin.WithSink(sink))

	item, err := service.Recover(context.Background(), "account-1")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if item.Pool != account.PoolReady || item.UnavailableReason != "" || !item.RetryAt.IsZero() {
		t.Fatalf("account = %#v", item)
	}
	if len(sink.items) != 1 || sink.items[0].Pool != account.PoolReady {
		t.Fatalf("sink = %#v", sink.items)
	}
}

func TestRecoverKeepsRejectedCredentialUnavailable(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"account-1": {ID: "account-1", AccessToken: "bad", Pool: account.PoolUnavailable},
	}}
	service := admin.NewService(repository, validator{reason: account.ReasonAuth}, admin.WithSink(&memorySink{}))

	item, err := service.Recover(context.Background(), "account-1")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonAuth {
		t.Fatalf("account = %#v", item)
	}
}

func TestRecoverMissingAccountReturnsError(t *testing.T) {
	service := admin.NewService(&memoryRepository{}, validator{})
	if _, err := service.Recover(context.Background(), "missing"); err == nil {
		t.Fatal("missing account should fail")
	}
}
