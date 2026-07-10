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
