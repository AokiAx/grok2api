package repository_test

import (
	"context"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type contractRepository struct{}

func (contractRepository) ListAccounts(context.Context) ([]account.Account, error) {
	return nil, nil
}

func (contractRepository) ListAccountsPage(
	context.Context,
	repository.ListAccountsQuery,
) (repository.ListAccountsResult, error) {
	return repository.ListAccountsResult{}, nil
}

func (contractRepository) AccountStats(context.Context) (repository.AccountStats, error) {
	return repository.AccountStats{}, nil
}

func (contractRepository) SaveAccount(context.Context, account.Account) error {
	return nil
}

func (contractRepository) DeleteAccount(context.Context, string) error {
	return nil
}

var (
	_ repository.AccountLister     = contractRepository{}
	_ repository.AccountSaver      = contractRepository{}
	_ repository.AccountStore      = contractRepository{}
	_ repository.AccountRepository = contractRepository{}
)

func TestAccountRepositoryDTOsCarryDomainAccounts(t *testing.T) {
	item := account.Account{ID: "account-1"}
	result := repository.ListAccountsResult{
		Items:    []account.Account{item},
		Total:    1,
		Page:     1,
		PageSize: 50,
	}

	if got := result.Items[0].ID; got != item.ID {
		t.Fatalf("result account ID = %q; want %q", got, item.ID)
	}
}
