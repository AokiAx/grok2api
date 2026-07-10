package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/scheduler"
)

func readyAccount(id string) account.Account {
	return account.Account{
		ID:        id,
		Pool:      account.PoolReady,
		MaxActive: 1,
	}
}

func TestReadyPoolUsesSimpleRoundRobin(t *testing.T) {
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
	})

	want := []string{"a", "b", "a", "b"}
	for index, expected := range want {
		lease, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", index, err)
		}
		if lease.Account().ID != expected {
			t.Fatalf("acquire %d = %q; want %q", index, lease.Account().ID, expected)
		}
		lease.Release()
	}
}

func TestSchedulerNeverSelectsUnavailableAccount(t *testing.T) {
	s := scheduler.New([]account.Account{
		{
			ID:                "quota",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			MaxActive:         1,
		},
		readyAccount("ready"),
	})

	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()
	if lease.Account().ID != "ready" {
		t.Fatalf("selected %q; want ready", lease.Account().ID)
	}
}

func TestMoveToUnavailableRemovesAccountFromReadyRotation(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
	})

	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	if lease.Account().ID != "a" {
		t.Fatalf("selected %q; want a", lease.Account().ID)
	}
	lease.MoveUnavailable(account.ReasonQuota, now.Add(time.Hour), "subscription:free-usage-exhausted")
	lease.Release()

	for range 3 {
		next, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire b: %v", err)
		}
		if next.Account().ID != "b" {
			t.Fatalf("selected %q after quota move; want b", next.Account().ID)
		}
		next.Release()
	}
}

func TestAcquireHonorsContextWhenReadyPoolIsBusy(t *testing.T) {
	s := scheduler.New([]account.Account{readyAccount("a")})
	first, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Acquire(ctx); err == nil {
		t.Fatal("expected canceled acquire to fail")
	}
}

func TestPromoteDueMovesRecoverableAccountsBackToReady(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	s := scheduler.New([]account.Account{
		{
			ID:                "quota-due",
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

	promoted := s.PromoteDue(now)

	if len(promoted) != 1 || promoted[0].ID != "quota-due" {
		t.Fatalf("promoted = %#v; want quota-due", promoted)
	}
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire promoted: %v", err)
	}
	defer lease.Release()
	if lease.Account().ID != "quota-due" {
		t.Fatalf("selected %q", lease.Account().ID)
	}
}

func TestStatusAndEarliestRetry(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	s := scheduler.New([]account.Account{
		readyAccount("ready"),
		{
			ID:                "quota",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			RetryAt:           now.Add(20 * time.Minute),
			MaxActive:         1,
		},
		{
			ID:                "auth",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonAuth,
			RetryAt:           now.Add(40 * time.Minute),
			MaxActive:         1,
		},
	})

	if s.ReadyCount() != 1 {
		t.Fatalf("ready count = %d", s.ReadyCount())
	}
	ready, unavailable, reasons := s.Status()
	if ready != 1 || unavailable != 2 {
		t.Fatalf("status = %d/%d", ready, unavailable)
	}
	if reasons[account.ReasonQuota] != 1 || reasons[account.ReasonAuth] != 1 {
		t.Fatalf("reasons = %#v", reasons)
	}
	if !s.EarliestRetry().Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("earliest retry = %s", s.EarliestRetry())
	}
}

func TestDeleteRemovesAccountAndAdvancesRevision(t *testing.T) {
	s := scheduler.New([]account.Account{readyAccount("a"), readyAccount("b")})
	revision := s.Revision()
	if !s.Delete("a") {
		t.Fatal("delete should report existing account")
	}
	if s.Revision() <= revision {
		t.Fatalf("revision did not advance: %d", s.Revision())
	}
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.Account().ID != "b" {
		t.Fatalf("selected = %q", lease.Account().ID)
	}
	lease.Release()
	if s.Delete("missing") {
		t.Fatal("missing delete should be false")
	}
}

func TestUpsertAndPromotionAdvanceRevision(t *testing.T) {
	now := time.Now().UTC()
	s := scheduler.New(nil)
	initial := s.Revision()
	s.Upsert(readyAccount("ready"))
	if s.Revision() <= initial {
		t.Fatal("upsert did not advance revision")
	}
	afterUpsert := s.Revision()
	s.Upsert(account.Account{
		ID:                "quota",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(-time.Second),
	})
	s.PromoteDue(now)
	if s.Revision() <= afterUpsert {
		t.Fatal("promotion did not advance revision")
	}
}
