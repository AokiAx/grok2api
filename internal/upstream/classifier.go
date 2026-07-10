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
// Only evaluates complete limit+remaining pairs; partial headers never exhaust.
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
	case isQuotaFailure(status, lowerCode, lowerMessage):
		failure.Kind = FailureQuota
		failure.Reason = account.ReasonQuota
		if failure.Code == "" {
			failure.Code = "subscription:free-usage-exhausted"
		}
		if matches := quotaPattern.FindStringSubmatch(message); len(matches) == 3 {
			failure.QuotaActual, _ = strconv.ParseInt(matches[1], 10, 64)
			failure.QuotaLimit, _ = strconv.ParseInt(matches[2], 10, 64)
		}
	case status == 429:
		failure.Kind = FailureRateLimit
		failure.Reason = account.ReasonCooldown
		if failure.Code == "" {
			failure.Code = "rate_limit"
		}
	case isAuthFailure(status, lowerCode, lowerMessage):
		failure.Kind = FailureAuth
		failure.Reason = account.ReasonAuth
		if failure.Code == "" {
			failure.Code = "auth_failed"
		}
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
	limitReq, hasLimitReq := headerInt64(header, "x-ratelimit-limit-requests")
	remainReq, hasRemainReq := headerInt64(header, "x-ratelimit-remaining-requests")
	// Require a complete pair. A lone limit with zero remaining default would
	// false-exhaust free accounts behind incomplete proxies.
	if hasLimitReq && hasRemainReq {
		usage.LimitRequests = limitReq
		usage.RemainingRequests = remainReq
		usage.HasRequests = true
	}
	limitTok, hasLimitTok := headerInt64(header, "x-ratelimit-limit-tokens")
	remainTok, hasRemainTok := headerInt64(header, "x-ratelimit-remaining-tokens")
	if hasLimitTok && hasRemainTok {
		usage.LimitTokens = limitTok
		usage.RemainingTokens = remainTok
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

func isQuotaFailure(status int, code, message string) bool {
	if status != 429 && status != 403 {
		// Some free-tier exhaustion responses arrive as 403/400-ish text payloads.
		if status < 400 {
			return false
		}
	}
	if code == "subscription:free-usage-exhausted" || strings.Contains(code, "usage-exhausted") {
		return true
	}
	markers := []string{
		"free usage",
		"free-usage",
		"usage exhausted",
		"usage-exhausted",
		"rolling 24-hour",
		"quota exceeded",
		"quota_exceeded",
		"insufficient_quota",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) || strings.Contains(code, marker) {
			return true
		}
	}
	return false
}

func isAuthFailure(status int, code, message string) bool {
	if status == 401 || status == 403 {
		// Prefer quota classification when the body clearly says usage exhausted.
		if isQuotaFailure(status, code, message) {
			return false
		}
		return true
	}
	markers := []string{
		"invalid or expired credentials",
		"invalid_token",
		"invalid token",
		"expired credentials",
		"no auth context",
		"unauthorized",
		"permissiondenied",
		"permission denied",
		"token has expired",
		"refresh_token",
	}
	combined := code + " " + message
	for _, marker := range markers {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	return false
}
