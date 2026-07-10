package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/runtime"
	"github.com/AokiAx/grok2api/internal/scheduler"
)

type recoveryStore struct {
	saved []account.Account
}

func (s *recoveryStore) SaveAccount(_ context.Context, item account.Account) error {
	s.saved = append(s.saved, item)
	return nil
}

func TestRecoverDuePromotesQuotaButNotAuth(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	s := scheduler.New([]account.Account{
		{
			ID:                "quota",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			RetryAt:           now.Add(-time.Minute),
			MaxActive:         1,
		},
		{
			ID:                "auth",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonAuth,
			RetryAt:           now.Add(-time.Minute),
			MaxActive:         1,
		},
	})
	store := &recoveryStore{}

	if err := runtime.RecoverDue(context.Background(), s, store, now); err != nil {
		t.Fatalf("recover due: %v", err)
	}
	if len(store.saved) != 1 || store.saved[0].ID != "quota" {
		t.Fatalf("saved = %#v; want quota", store.saved)
	}
}

func TestRunRecoveryStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runtime.RunRecovery(
		ctx,
		scheduler.New(nil),
		&recoveryStore{},
		time.Hour,
	)
	if err != nil {
		t.Fatalf("run recovery: %v", err)
	}
}
