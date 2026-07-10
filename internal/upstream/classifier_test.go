package upstream_test

import (
	"net/http"
	"testing"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/upstream"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantKind   upstream.FailureKind
		wantReason account.UnavailableReason
		wantCode   string
	}{
		{
			name:       "rolling quota exhaustion",
			status:     429,
			body:       `{"code":"subscription:free-usage-exhausted","error":"rolling 24-hour window — tokens (actual/limit): 1074920/1000000"}`,
			wantKind:   upstream.FailureQuota,
			wantReason: account.ReasonQuota,
			wantCode:   "subscription:free-usage-exhausted",
		},
		{
			name:       "ordinary rate limit",
			status:     429,
			body:       `{"code":"rate-limit","error":"too many requests"}`,
			wantKind:   upstream.FailureRateLimit,
			wantReason: account.ReasonCooldown,
			wantCode:   "rate-limit",
		},
		{
			name:       "authentication failure",
			status:     401,
			body:       `{"code":"invalid-token"}`,
			wantKind:   upstream.FailureAuth,
			wantReason: account.ReasonAuth,
			wantCode:   "invalid-token",
		},
		{
			name:     "upstream server error",
			status:   503,
			body:     `service unavailable`,
			wantKind: upstream.FailureUpstream,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upstream.ClassifyFailure(tt.status, []byte(tt.body))
			if got.Kind != tt.wantKind {
				t.Fatalf("Kind = %q; want %q", got.Kind, tt.wantKind)
			}
			if got.Reason != tt.wantReason {
				t.Fatalf("Reason = %q; want %q", got.Reason, tt.wantReason)
			}
			if got.Code != tt.wantCode {
				t.Fatalf("Code = %q; want %q", got.Code, tt.wantCode)
			}
		})
	}
}

func TestQuotaFailureExtractsUsage(t *testing.T) {
	failure := upstream.ClassifyFailure(
		429,
		[]byte(`{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 1023321/1000000"}`),
	)

	if failure.QuotaActual != 1_023_321 || failure.QuotaLimit != 1_000_000 {
		t.Fatalf(
			"quota = %d/%d; want 1023321/1000000",
			failure.QuotaActual,
			failure.QuotaLimit,
		)
	}
}

func TestParseRateLimitHeadersPrefersTokens(t *testing.T) {
	header := make(http.Header)
	header.Set("x-ratelimit-limit-requests", "21")
	header.Set("x-ratelimit-remaining-requests", "18")
	header.Set("x-ratelimit-limit-tokens", "1000000")
	header.Set("x-ratelimit-remaining-tokens", "750000")

	usage := upstream.ParseRateLimitHeaders(header)
	if !usage.Present() {
		t.Fatal("expected usage present")
	}
	if usage.QuotaLimit() != 1_000_000 {
		t.Fatalf("limit = %d; want tokens limit", usage.QuotaLimit())
	}
	if usage.QuotaActual() != 250_000 {
		t.Fatalf("actual = %d; want 250000 used tokens", usage.QuotaActual())
	}
	if usage.Exhausted() {
		t.Fatal("should not be exhausted")
	}
}

func TestParseRateLimitHeadersExhaustedRemainingZero(t *testing.T) {
	header := make(http.Header)
	header.Set("x-ratelimit-limit-tokens", "1000000")
	header.Set("x-ratelimit-remaining-tokens", "0")
	usage := upstream.ParseRateLimitHeaders(header)
	if !usage.Exhausted() {
		t.Fatal("remaining 0 should be exhausted")
	}
	if usage.QuotaActual() != 1_000_000 || usage.QuotaLimit() != 1_000_000 {
		t.Fatalf("quota = %d/%d", usage.QuotaActual(), usage.QuotaLimit())
	}
}

func TestParseRateLimitHeadersIgnoresMissing(t *testing.T) {
	if usage := upstream.ParseRateLimitHeaders(http.Header{}); usage.Present() {
		t.Fatalf("empty headers should not present: %#v", usage)
	}
}
