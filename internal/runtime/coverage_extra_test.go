package runtime_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/runtime"
	"github.com/AokiAx/grok2api/internal/scheduler"
)

type usageProbe struct {
	reason account.UnavailableReason
	code   string
	actual int64
	limit  int64
	has    bool
	err    error
	calls  atomic.Int64
}

func (p *usageProbe) ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error) {
	p.calls.Add(1)
	return p.reason, p.code, p.err
}

func (p *usageProbe) ProbeFreeQuotaUsage(context.Context, account.Account) (account.UnavailableReason, string, int64, int64, bool, error) {
	p.calls.Add(1)
	return p.reason, p.code, p.actual, p.limit, p.has, p.err
}

func TestRecoverQuotaWritesUsageAndCapsProbeBudget(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	const n = 300
	accounts := make([]account.Account, 0, n)
	for i := 0; i < n; i++ {
		accounts = append(accounts, account.Account{
			ID:                fmt.Sprintf("q%d", i),
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			RetryAt:           now.Add(-time.Minute),
			QuotaActual:       100,
			QuotaLimit:        100,
			MaxActive:         1,
		})
	}
	store := &recoveryStore{accounts: accounts}
	pool := scheduler.New(accounts)
	prober := &usageProbe{actual: 10, limit: 100, has: true}
	result, err := runtime.RecoverQuota(context.Background(), pool, store, prober, nil, now, 0, nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	// Budget is 256 probes/tick for large due queues.
	const want = 256
	if prober.calls.Load() != want {
		t.Fatalf("calls=%d; want max %d", prober.calls.Load(), want)
	}
	if result.Recovered != want || result.Skipped < n-want {
		t.Fatalf("result=%#v", result)
	}
	found := false
	for _, item := range store.saved {
		if item.Pool == account.PoolReady && item.QuotaActual == 10 && item.QuotaLimit == 100 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("saved=%#v", store.saved)
	}
}

func TestRecoverQuotaDeferredKeepsUsage(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	item := account.Account{
		ID:                "q1",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(-time.Minute),
		MaxActive:         1,
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	pool := scheduler.New([]account.Account{item})
	prober := &usageProbe{
		reason: account.ReasonQuota,
		code:   "subscription:free-usage-exhausted",
		actual: 100,
		limit:  100,
		has:    true,
	}
	result, err := runtime.RecoverQuota(context.Background(), pool, store, prober, nil, now, time.Hour, nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Deferred != 1 || len(store.saved) != 1 {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
	if store.saved[0].QuotaActual != 100 || store.saved[0].QuotaLimit != 100 {
		t.Fatalf("usage not kept: %#v", store.saved[0])
	}
}

func TestRecoverCredentialsValidationFailure(t *testing.T) {
	now := time.Now().UTC()
	expired := account.Account{
		ID: "expired", AccessToken: "old", RefreshToken: "refresh",
		OIDCIssuer: "https://auth.x.ai", OIDCClientID: "client",
		Pool: account.PoolUnavailable, UnavailableReason: account.ReasonAuth,
	}
	refreshed := expired
	refreshed.AccessToken = "new"
	store := &recoveryStore{accounts: []account.Account{expired}}
	pool := scheduler.New([]account.Account{expired})
	result, err := runtime.RecoverCredentials(
		context.Background(), pool, store,
		credentialRefresher{item: refreshed},
		credentialValidator{reason: account.ReasonAuth, code: "still-bad"},
		now,
	)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Failed != 1 || len(store.saved) != 1 {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
	if store.saved[0].Pool != account.PoolUnavailable || store.saved[0].LastErrorCode != "still-bad" {
		t.Fatalf("saved=%#v", store.saved[0])
	}
}

func TestRecoverDuePromotesCooldownDespiteSaveNoise(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	s := scheduler.New([]account.Account{{
		ID:                "c1",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonCooldown,
		RetryAt:           now.Add(-time.Minute),
		MaxActive:         1,
	}})
	store := &recoveryStore{}
	if err := runtime.RecoverDue(context.Background(), s, store, now); err != nil {
		t.Fatalf("recover due: %v", err)
	}
	if len(store.saved) != 1 || store.saved[0].ID != "c1" || store.saved[0].Pool != account.PoolReady {
		t.Fatalf("saved=%#v", store.saved)
	}
}
