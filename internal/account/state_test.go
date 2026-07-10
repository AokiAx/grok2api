package account_test

import (
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
)

func TestAccountAvailabilityUsesTwoPools(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		account   account.Account
		available bool
	}{
		{
			name: "ready account is available",
			account: account.Account{
				ID:        "ready",
				Pool:      account.PoolReady,
				MaxActive: 1,
			},
			available: true,
		},
		{
			name: "quota account is unavailable",
			account: account.Account{
				ID:                "quota",
				Pool:              account.PoolUnavailable,
				UnavailableReason: account.ReasonQuota,
				RetryAt:           now.Add(-time.Minute),
				MaxActive:         1,
			},
			available: false,
		},
		{
			name: "ready account at concurrency limit is unavailable",
			account: account.Account{
				ID:        "busy",
				Pool:      account.PoolReady,
				Active:    1,
				MaxActive: 1,
			},
			available: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.Available(now); got != tt.available {
				t.Fatalf("Available() = %v; want %v", got, tt.available)
			}
		})
	}
}

func TestUnavailableReasonsRemainExplicit(t *testing.T) {
	reasons := []account.UnavailableReason{
		account.ReasonQuota,
		account.ReasonAuth,
		account.ReasonCooldown,
		account.ReasonValidating,
		account.ReasonDisabled,
	}

	seen := map[account.UnavailableReason]bool{}
	for _, reason := range reasons {
		if reason == "" {
			t.Fatal("unavailable reason must not be empty")
		}
		if seen[reason] {
			t.Fatalf("duplicate unavailable reason %q", reason)
		}
		seen[reason] = true
	}
}
