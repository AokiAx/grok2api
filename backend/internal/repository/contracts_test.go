package repository_test

import (
	"context"
	"testing"
	"time"

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

func (contractRepository) GetAccount(context.Context, string) (account.Account, bool, error) {
	return account.Account{}, false, nil
}

func (contractRepository) SaveAccounts(context.Context, []account.Account) error {
	return nil
}

func (contractRepository) DeleteAccounts(context.Context, []string) error {
	return nil
}

func (contractRepository) ListAccountEvents(
	context.Context,
	repository.ListAccountEventsQuery,
) (repository.ListAccountEventsResult, error) {
	return repository.ListAccountEventsResult{}, nil
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

func TestAccountEventDTOCarriesAdministrativeTimelineData(t *testing.T) {
	createdAt := time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC)
	event := repository.AccountEvent{
		ID:        42,
		AccountID: "account-1",
		Type:      repository.AccountEventConfiguration,
		FromPool:  account.PoolReady,
		ToPool:    account.PoolReady,
		Reason:    "admin-update",
		ErrorCode: "",
		CreatedAt: createdAt,
		Details: map[string]any{
			"priority":   25,
			"max_active": 4,
		},
	}
	result := repository.ListAccountEventsResult{Items: []repository.AccountEvent{event}, Total: 1, Page: 1, PageSize: 20}

	if result.Items[0].Type != repository.AccountEventConfiguration || result.Items[0].Details["priority"] != 25 {
		t.Fatalf("event result = %#v", result)
	}
}
