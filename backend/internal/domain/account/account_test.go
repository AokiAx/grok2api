package account

import (
	"testing"
	"time"
)

func TestMarkUnavailableOverwritesTransitionState(t *testing.T) {
	at := time.Date(2026, 7, 15, 12, 30, 0, 0, time.FixedZone("HKT", 8*60*60))
	retryAt := at.Add(5 * time.Minute)
	item := Account{
		Pool:              PoolReady,
		UnavailableReason: ReasonCooldown,
		RetryAt:           at.Add(time.Hour),
		LastErrorCode:     "old",
	}

	item.MarkUnavailable(ReasonAuth, retryAt, "refresh-failed", at)

	if item.Pool != PoolUnavailable || item.UnavailableReason != ReasonAuth {
		t.Fatalf("unexpected state: pool=%q reason=%q", item.Pool, item.UnavailableReason)
	}
	if !item.RetryAt.Equal(retryAt) || item.LastErrorCode != "refresh-failed" {
		t.Fatalf("unexpected retry/error: retry=%v code=%q", item.RetryAt, item.LastErrorCode)
	}
	if !item.UpdatedAt.Equal(at.UTC()) || item.UpdatedAt.Location() != time.UTC {
		t.Fatalf("UpdatedAt=%v; want UTC %v", item.UpdatedAt, at.UTC())
	}
}

func TestReadyTransitionsPreserveScenarioSpecificState(t *testing.T) {
	at := time.Date(2026, 7, 15, 4, 30, 0, 0, time.UTC)
	base := Account{
		Pool:                PoolUnavailable,
		UnavailableReason:   ReasonAuth,
		RetryAt:             at.Add(time.Hour),
		LastErrorCode:       "auth",
		AuthenticationFails: 7,
		QuotaActual:         90,
		QuotaLimit:          100,
	}

	t.Run("generic ready preserves counters", func(t *testing.T) {
		item := base
		item.MarkReady(at)
		assertReady(t, item, at)
		if item.AuthenticationFails != 7 || item.QuotaActual != 90 {
			t.Fatalf("generic ready changed counters: %+v", item)
		}
	})

	t.Run("validated recovery resets auth failures", func(t *testing.T) {
		item := base
		item.RecoverValidated(at)
		assertReady(t, item, at)
		if item.AuthenticationFails != 0 || item.QuotaActual != 90 {
			t.Fatalf("validated recovery counters: fails=%d quota=%d", item.AuthenticationFails, item.QuotaActual)
		}
	})

	t.Run("quota window recovery resets only actual usage", func(t *testing.T) {
		item := base
		item.RecoverQuotaWindow(at)
		assertReady(t, item, at)
		if item.QuotaActual != 0 || item.QuotaLimit != 100 || item.AuthenticationFails != 7 {
			t.Fatalf("quota recovery counters: %+v", item)
		}
	})
}

func TestParkKnownExhaustedPreservesUsefulMetadata(t *testing.T) {
	now := time.Date(2026, 7, 15, 4, 30, 0, 0, time.UTC)

	t.Run("adds defaults", func(t *testing.T) {
		item := Account{Pool: PoolReady, QuotaActual: 100, QuotaLimit: 100}
		if !item.ParkKnownExhausted(now, 24*time.Hour) {
			t.Fatal("expected exhausted ready account to be parked")
		}
		if item.Pool != PoolUnavailable || item.UnavailableReason != ReasonQuota {
			t.Fatalf("unexpected state: %+v", item)
		}
		if item.LastErrorCode != "local:quota-exhausted" || !item.RetryAt.Equal(now.Add(24*time.Hour)) {
			t.Fatalf("unexpected metadata: %+v", item)
		}
	})

	t.Run("preserves code and later retry", func(t *testing.T) {
		later := now.Add(48 * time.Hour)
		item := Account{
			Pool:          PoolReady,
			QuotaActual:   100,
			QuotaLimit:    100,
			LastErrorCode: "subscription:known",
			RetryAt:       later,
		}
		if !item.ParkKnownExhausted(now, 24*time.Hour) {
			t.Fatal("expected exhausted ready account to be parked")
		}
		if item.LastErrorCode != "subscription:known" || !item.RetryAt.Equal(later) {
			t.Fatalf("existing metadata was lost: %+v", item)
		}
	})

	t.Run("ignores ineligible account", func(t *testing.T) {
		item := Account{Pool: PoolUnavailable, QuotaActual: 100, QuotaLimit: 100}
		before := item
		if item.ParkKnownExhausted(now, 24*time.Hour) {
			t.Fatal("did not expect already unavailable account to be parked")
		}
		if item != before {
			t.Fatalf("ineligible account changed: before=%+v after=%+v", before, item)
		}
	})
}

func TestRecoverCooldownRequiresDueCooldown(t *testing.T) {
	now := time.Date(2026, 7, 15, 4, 30, 0, 0, time.UTC)
	tests := []struct {
		name    string
		item    Account
		wantDue bool
	}{
		{"due", Account{Pool: PoolUnavailable, UnavailableReason: ReasonCooldown, RetryAt: now}, true},
		{"zero retry remains parked", Account{Pool: PoolUnavailable, UnavailableReason: ReasonCooldown}, false},
		{"future", Account{Pool: PoolUnavailable, UnavailableReason: ReasonCooldown, RetryAt: now.Add(time.Second)}, false},
		{"wrong reason", Account{Pool: PoolUnavailable, UnavailableReason: ReasonAuth, RetryAt: now}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := tt.item
			got := item.RecoverCooldown(now)
			if got != tt.wantDue {
				t.Fatalf("RecoverCooldown()=%v; want %v", got, tt.wantDue)
			}
			if got {
				assertReady(t, item, now)
			}
		})
	}
}

func TestRecordUsageNormalizesTimestamp(t *testing.T) {
	t.Run("explicit timestamp", func(t *testing.T) {
		at := time.Date(2026, 7, 15, 12, 30, 0, 0, time.FixedZone("HKT", 8*60*60))
		item := Account{}
		item.RecordUsage(25, 100, at)
		if item.QuotaActual != 25 || item.QuotaLimit != 100 {
			t.Fatalf("quota=(%d,%d); want (25,100)", item.QuotaActual, item.QuotaLimit)
		}
		if !item.LastSuccessAt.Equal(at.UTC()) || !item.UpdatedAt.Equal(at.UTC()) {
			t.Fatalf("timestamps=%v/%v; want %v", item.LastSuccessAt, item.UpdatedAt, at.UTC())
		}
		if item.LastSuccessAt.Location() != time.UTC || item.UpdatedAt.Location() != time.UTC {
			t.Fatalf("timestamps were not normalized to UTC")
		}
	})

	t.Run("zero timestamp becomes current UTC time", func(t *testing.T) {
		before := time.Now().UTC()
		item := Account{}
		item.RecordUsage(1, 2, time.Time{})
		after := time.Now().UTC()
		if item.LastSuccessAt.Before(before) || item.LastSuccessAt.After(after) {
			t.Fatalf("LastSuccessAt=%v; want between %v and %v", item.LastSuccessAt, before, after)
		}
		if item.LastSuccessAt.Location() != time.UTC || !item.UpdatedAt.Equal(item.LastSuccessAt) {
			t.Fatalf("zero timestamp normalization failed: %+v", item)
		}
	})
}

func TestRefreshAndValidationFailuresAreAtomic(t *testing.T) {
	now := time.Date(2026, 7, 15, 4, 30, 0, 0, time.UTC)

	t.Run("transient refresh", func(t *testing.T) {
		item := Account{Pool: PoolReady, AuthenticationFails: 2}
		item.ApplyRefreshFailure(false, now, 5*time.Minute)
		if item.AuthenticationFails != 3 || item.Pool != PoolUnavailable || item.UnavailableReason != ReasonAuth {
			t.Fatalf("unexpected transient refresh state: %+v", item)
		}
		if item.LastErrorCode != "refresh-failed" || !item.RetryAt.Equal(now.Add(5*time.Minute)) {
			t.Fatalf("unexpected transient refresh metadata: %+v", item)
		}
	})

	t.Run("permanent refresh", func(t *testing.T) {
		item := Account{Pool: PoolReady, AuthenticationFails: 2}
		item.ApplyRefreshFailure(true, now, 5*time.Minute)
		if item.AuthenticationFails != 3 || item.UnavailableReason != ReasonDisabled {
			t.Fatalf("unexpected permanent refresh state: %+v", item)
		}
		if item.LastErrorCode != "refresh-revoked" || !item.RetryAt.Equal(now.Add(365*24*time.Hour)) {
			t.Fatalf("unexpected permanent refresh metadata: %+v", item)
		}
	})

	t.Run("revoked isolation preserves supplied permanent code", func(t *testing.T) {
		item := Account{LastErrorCode: "invalid_grant"}
		item.DisableRevoked(now, item.LastErrorCode)
		if item.LastErrorCode != "invalid_grant" {
			t.Fatalf("LastErrorCode=%q; want preserved permanent code", item.LastErrorCode)
		}
	})

	t.Run("validation failure increments before caller threshold decision", func(t *testing.T) {
		item := Account{Pool: PoolUnavailable, UnavailableReason: ReasonValidating, AuthenticationFails: 11}
		fails := item.RecordValidationFailure("permission-denied", now.Add(45*time.Second), now)
		if fails != 12 || item.AuthenticationFails != 12 {
			t.Fatalf("fails=%d/%d; want 12", fails, item.AuthenticationFails)
		}
		if item.Pool != PoolUnavailable || item.UnavailableReason != ReasonValidating {
			t.Fatalf("validation counter changed pool state: %+v", item)
		}
	})
}

func assertReady(t *testing.T, item Account, at time.Time) {
	t.Helper()
	if item.Pool != PoolReady || item.UnavailableReason != "" || !item.RetryAt.IsZero() || item.LastErrorCode != "" {
		t.Fatalf("account is not ready: %+v", item)
	}
	if !item.UpdatedAt.Equal(at.UTC()) {
		t.Fatalf("UpdatedAt=%v; want %v", item.UpdatedAt, at.UTC())
	}
}
