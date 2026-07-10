package upstream

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/AokiAx/grok2api/internal/account"
)

type FailureKind string

const (
	FailureQuota     FailureKind = "quota"
	FailureRateLimit FailureKind = "rate_limit"
	FailureAuth      FailureKind = "auth"
	FailureRequest   FailureKind = "request"
	FailureUpstream  FailureKind = "upstream"
)

type Failure struct {
	Kind        FailureKind
	Reason      account.UnavailableReason
	Code        string
	Message     string
	QuotaActual int64
	QuotaLimit  int64
}

// RateLimitUsage captures free-tier remaining capacity from response headers.
// Grok free accounts do not expose a reliable billing query; usage is only
// visible via x-ratelimit-* headers on real chat/stream responses.
type RateLimitUsage struct {
	LimitRequests     int64
	RemainingRequests int64
	LimitTokens       int64
	RemainingTokens   int64
	HasRequests       bool
	HasTokens         bool
}

func (u RateLimitUsage) Present() bool {
	return u.HasRequests || u.HasTokens
}

// QuotaActual returns used amount for persistence.
// Prefer token counters when present; otherwise fall back to request counters.
func (u RateLimitUsage) QuotaActual() int64 {
	if u.HasTokens {
		used := u.LimitTokens - u.RemainingTokens
		if used < 0 {
			return 0
		}
		return used
	}
	if u.HasRequests {
		used := u.LimitRequests - u.RemainingRequests
		if used < 0 {
			return 0
		}
		return used
	}
	return 0
}

// QuotaLimit returns the limit for the preferred counter.
func (u RateLimitUsage) QuotaLimit() int64 {
	if u.HasTokens {
		return u.LimitTokens
	}
	if u.HasRequests {
		return u.LimitRequests
	}
	return 0
}

// Exhausted reports whether free capacity is fully consumed.
func (u RateLimitUsage) Exhausted() bool {
	if u.HasTokens && u.RemainingTokens <= 0 && u.LimitTokens > 0 {
		return true
	}
	if u.HasRequests && u.RemainingRequests <= 0 && u.LimitRequests > 0 {
		return true
	}
	return false
}

var quotaPattern = regexp.MustCompile(`(?i)actual/limit\)?:\s*(\d+)\s*/\s*(\d+)`)

func ClassifyFailure(status int, body []byte) Failure {
	payload := struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}{}
	_ = json.Unmarshal(body, &payload)
	message := payload.Error
	if message == "" {
		message = string(body)
	}

	failure := Failure{Code: payload.Code, Message: message}
	lowerCode := strings.ToLower(payload.Code)
	lowerMessage := strings.ToLower(message)

	switch {
	case status == 429 && (lowerCode == "subscription:free-usage-exhausted" || strings.Contains(lowerMessage, "free usage") || strings.Contains(lowerMessage, "rolling 24-hour")):
		failure.Kind = FailureQuota
		failure.Reason = account.ReasonQuota
		if matches := quotaPattern.FindStringSubmatch(message); len(matches) == 3 {
			failure.QuotaActual, _ = strconv.ParseInt(matches[1], 10, 64)
			failure.QuotaLimit, _ = strconv.ParseInt(matches[2], 10, 64)
		}
	case status == 429:
		failure.Kind = FailureRateLimit
		failure.Reason = account.ReasonCooldown
	case status == 401 || status == 403:
		failure.Kind = FailureAuth
		failure.Reason = account.ReasonAuth
	case status >= 500:
		failure.Kind = FailureUpstream
	case status >= 400:
		failure.Kind = FailureRequest
	default:
		failure.Kind = FailureUpstream
	}

	return failure
}

// ParseRateLimitHeaders extracts free-tier usage from upstream response headers.
// Known headers (observed from Grok CLI / chat proxy):
//   - x-ratelimit-limit-requests
//   - x-ratelimit-remaining-requests
//   - x-ratelimit-limit-tokens
//   - x-ratelimit-remaining-tokens
func ParseRateLimitHeaders(header http.Header) RateLimitUsage {
	if header == nil {
		return RateLimitUsage{}
	}
	usage := RateLimitUsage{}
	if value, ok := headerInt64(header, "x-ratelimit-limit-requests"); ok {
		usage.LimitRequests = value
		usage.HasRequests = true
	}
	if value, ok := headerInt64(header, "x-ratelimit-remaining-requests"); ok {
		usage.RemainingRequests = value
		usage.HasRequests = true
	}
	if value, ok := headerInt64(header, "x-ratelimit-limit-tokens"); ok {
		usage.LimitTokens = value
		usage.HasTokens = true
	}
	if value, ok := headerInt64(header, "x-ratelimit-remaining-tokens"); ok {
		usage.RemainingTokens = value
		usage.HasTokens = true
	}
	return usage
}

func headerInt64(header http.Header, key string) (int64, bool) {
	raw := strings.TrimSpace(header.Get(key))
	if raw == "" {
		return 0, false
	}
	// Some proxies may append units; take leading integer.
	for i, r := range raw {
		if r < '0' || r > '9' {
			if i == 0 {
				return 0, false
			}
			raw = raw[:i]
			break
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
