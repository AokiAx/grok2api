package scheduler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/account"
	"github.com/AokiAx/grok2api/backend/internal/scheduler"
)

func readyAccount(id string) account.Account {
	return account.Account{
		ID:        id,
		Pool:      account.PoolReady,
		MaxActive: 1,
	}
}

func TestReadyPoolUsesRoundRobin(t *testing.T) {
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
	}).WithStrategy(scheduler.StrategyRoundRobin)

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

func TestFillFirstBurnsOneAccountBeforeNext(t *testing.T) {
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
		readyAccount("c"),
	}).WithStrategy(scheduler.StrategyFillFirst)

	for i := 0; i < 5; i++ {
		lease, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if lease.Account().ID != "a" {
			t.Fatalf("fill-first %d = %q; want a", i, lease.Account().ID)
		}
		lease.Release()
	}
}

func TestHotSetCapsConcurrentFanOut(t *testing.T) {
	// active_size=2, max_active=1: only 2 accounts serve; cold stay unused.
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
		readyAccount("c"),
		readyAccount("d"),
		readyAccount("e"),
	}).WithStrategy(scheduler.StrategyRoundRobin).ApplyActiveSize(2)

	var held []*scheduler.Lease
	for i := 0; i < 2; i++ {
		lease, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire hot %d: %v", i, err)
		}
		held = append(held, lease)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx); err == nil {
		t.Fatal("expected no third lease while hot set is full and busy")
	}
	seen := map[string]struct{}{}
	for _, lease := range held {
		seen[lease.Account().ID] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("held %#v; want exactly 2 hot accounts", seen)
	}
	// Releasing and acquiring many times must never expand beyond hot set of 2.
	for _, lease := range held {
		lease.Release()
	}
	all := map[string]int{}
	for i := 0; i < 20; i++ {
		lease, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatalf("loop %d: %v", i, err)
		}
		all[lease.Account().ID]++
		lease.Release()
	}
	if len(all) != 2 {
		t.Fatalf("after release loop used %#v; want still only hot set of 2", all)
	}
}

func TestHotSetRoundRobinSpreadsLoad(t *testing.T) {
	// Within hot set of 3, RR must use all 3 — not stack on one account.
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
		readyAccount("c"),
		readyAccount("d"),
	}).WithStrategy(scheduler.StrategyRoundRobin).ApplyActiveSize(3)

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		lease, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		seen[lease.Account().ID]++
		lease.Release()
	}
	if len(seen) != 3 {
		t.Fatalf("used %#v; want exactly hot set of 3", seen)
	}
	if _, ok := seen["d"]; ok {
		t.Fatal("cold account d received traffic")
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

func TestAcquireStickyPrefersSameAccount(t *testing.T) {
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
		readyAccount("c"),
	})
	s.WithSticky(true, time.Hour)

	first, err := s.AcquireSticky(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	want := first.Account().ID
	first.Release()

	for i := 0; i < 5; i++ {
		lease, err := s.AcquireSticky(context.Background(), "user-1")
		if err != nil {
			t.Fatalf("sticky %d: %v", i, err)
		}
		if lease.Account().ID != want {
			t.Fatalf("sticky %d = %q; want %q", i, lease.Account().ID, want)
		}
		lease.Release()
	}

	// Different sticky key may pick another account (not forced equal).
	other, err := s.AcquireSticky(context.Background(), "user-2")
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	other.Release()
}

func TestAcquireStickyFallsBackWhenPreferredBusy(t *testing.T) {
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
	})
	s.WithSticky(true, time.Hour)

	first, err := s.AcquireSticky(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Hold sticky account busy.
	second, err := s.AcquireSticky(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if second.Account().ID == first.Account().ID {
		t.Fatal("expected fallback to another ready account while sticky is busy")
	}
	second.Release()
	first.Release()
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
			ID:                "cooldown-due",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonCooldown,
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

	if len(promoted) != 1 || promoted[0].ID != "cooldown-due" {
		t.Fatalf("promoted = %#v; want only cooldown-due", promoted)
	}
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire promoted: %v", err)
	}
	defer lease.Release()
	if lease.Account().ID != "cooldown-due" {
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
		ID:                "cooldown",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonCooldown,
		RetryAt:           now.Add(-time.Second),
	})
	s.PromoteDue(now)
	if s.Revision() <= afterUpsert {
		t.Fatal("promotion did not advance revision")
	}
}

func TestRecordUsageUpdatesQuotaAndSuccessTime(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	s := scheduler.New([]account.Account{readyAccount("a")})
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.RecordUsage(10, 100, now)
	item := lease.Account()
	lease.Release()
	if item.QuotaActual != 10 || item.QuotaLimit != 100 || item.LastSuccessAt.IsZero() {
		t.Fatalf("item=%#v", item)
	}
}

func TestApplyMaxActiveOverridesPerAccount(t *testing.T) {
	s := scheduler.New([]account.Account{
		readyAccount("a"),
		readyAccount("b"),
	})
	s.ApplyMaxActive(2)
	// Two leases on same account should succeed when MaxActive=2.
	first, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Account().ID != "a" && first.Account().ID != "b" {
		t.Fatalf("unexpected id %q", first.Account().ID)
	}
	// Pin sticky-less RR: hold first, acquire until we get same id or prove capacity.
	// Directly check Available via second acquire on full pool of 2 accounts with max 2 each
	// allows 4 concurrent; take two on first account by Prefer... simpler: use Apply then
	// acquire twice without release — with 2 accounts max 2, always get a lease.
	second, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	// Force both on same account: release second, set only one ready with max 2.
	first.Release()
	second.Release()

	s = scheduler.New([]account.Account{readyAccount("solo")})
	s.ApplyMaxActive(2)
	l1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("l1: %v", err)
	}
	l2, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("l2 with max_active=2: %v", err)
	}
	if l1.Account().ID != "solo" || l2.Account().ID != "solo" {
		t.Fatalf("want both solo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx); err == nil {
		t.Fatal("third lease should block/fail while max_active=2")
	}
	l1.Release()
	l2.Release()
}

func TestActiveByIDReturnsLiveLeaseCounts(t *testing.T) {
	s := scheduler.New([]account.Account{readyAccount("a"), readyAccount("b")})
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	active := s.ActiveByID()
	if active[lease.Account().ID] != 1 {
		t.Fatalf("active = %#v", active)
	}
	lease.Release()
	if len(s.ActiveByID()) != 0 {
		t.Fatalf("active after release = %#v", s.ActiveByID())
	}
}

func TestSelectionErrorReasons(t *testing.T) {
	// Empty pool → no_ready (or quota if only unavailable quota accounts).
	s := scheduler.New(nil)
	_, err := s.Acquire(context.Background())
	sel, ok := scheduler.AsSelectionError(err)
	if !ok {
		t.Fatalf("err=%v want SelectionError", err)
	}
	if sel.Reason != scheduler.SelectionNoReady {
		t.Fatalf("reason=%q", sel.Reason)
	}
	if !errors.Is(err, scheduler.ErrNoReadyAccount) {
		t.Fatal("expected errors.Is ErrNoReadyAccount")
	}

	s = scheduler.New([]account.Account{{
		ID: "q", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota,
		RetryAt: time.Now().Add(time.Hour), MaxActive: 1,
	}})
	_, err = s.Acquire(context.Background())
	sel, ok = scheduler.AsSelectionError(err)
	if !ok || sel.Reason != scheduler.SelectionQuota {
		t.Fatalf("quota reason: ok=%v err=%v sel=%#v", ok, err, sel)
	}
	if sel.RetryAfter < time.Second {
		t.Fatalf("retry after %s", sel.RetryAfter)
	}

	// Saturated: ready accounts exist but all busy / hot set full.
	s = scheduler.New([]account.Account{readyAccount("a")}).ApplyActiveSize(1)
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lease.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = s.Acquire(ctx)
	sel, ok = scheduler.AsSelectionError(err)
	if !ok || sel.Reason != scheduler.SelectionSaturated {
		t.Fatalf("saturated: ok=%v err=%v sel=%#v", ok, err, sel)
	}
}
