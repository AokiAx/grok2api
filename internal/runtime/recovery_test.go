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
	accounts []account.Account
	saved    []account.Account
}

func (s *recoveryStore) SaveAccount(_ context.Context, item account.Account) error {
	s.saved = append(s.saved, item)
	return nil
}

func (s *recoveryStore) ListAccounts(context.Context) ([]account.Account, error) {
	return append([]account.Account(nil), s.accounts...), nil
}

type credentialRefresher struct {
	item account.Account
	err  error
}

func (r credentialRefresher) Refresh(context.Context, account.Account) (account.Account, error) {
	return r.item, r.err
}

type credentialValidator struct {
	reason account.UnavailableReason
	code   string
}

func (v credentialValidator) Validate(context.Context, account.Account) (account.UnavailableReason, string, error) {
	return v.reason, v.code, nil
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

func TestRecoverCredentialsRefreshesAndRestoresAuthAccount(t *testing.T) {
	now := time.Now().UTC()
	expired := account.Account{
		ID: "expired", AccessToken: "old", RefreshToken: "refresh",
		OIDCIssuer: "https://auth.x.ai", OIDCClientID: "client",
		Pool: account.PoolUnavailable, UnavailableReason: account.ReasonAuth,
	}
	refreshed := expired
	refreshed.AccessToken = "new"
	refreshed.ExpiresAt = now.Add(time.Hour)
	store := &recoveryStore{accounts: []account.Account{expired}}
	pool := scheduler.New([]account.Account{expired})

	result, err := runtime.RecoverCredentials(
		context.Background(), pool, store,
		credentialRefresher{item: refreshed}, credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover credentials: %v", err)
	}
	if result.Recovered != 1 || result.Failed != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(store.saved) != 1 || store.saved[0].Pool != account.PoolReady || store.saved[0].AccessToken != "new" {
		t.Fatalf("saved = %#v", store.saved)
	}
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire refreshed: %v", err)
	}
	lease.Release()
}

func TestRecoverCredentialsBacksOffRejectedRefresh(t *testing.T) {
	now := time.Now().UTC()
	expired := account.Account{
		ID: "expired", RefreshToken: "bad", Pool: account.PoolUnavailable,
		UnavailableReason: account.ReasonAuth,
	}
	store := &recoveryStore{accounts: []account.Account{expired}}
	result, err := runtime.RecoverCredentials(
		context.Background(), scheduler.New([]account.Account{expired}), store,
		credentialRefresher{err: context.DeadlineExceeded}, credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover credentials: %v", err)
	}
	if result.Failed != 1 || len(store.saved) != 1 {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
	if !store.saved[0].RetryAt.After(now.Add(20*time.Minute)) || store.saved[0].LastErrorCode != "refresh-failed" {
		t.Fatalf("saved = %#v", store.saved[0])
	}
}
