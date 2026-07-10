package admin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
)

type memoryRepository struct {
	accounts map[string]account.Account
	saves    int
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
