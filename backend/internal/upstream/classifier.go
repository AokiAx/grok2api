package upstream

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
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
	// ModelScoped is true when the body points at free usage for a specific model
	// rather than a global account quota window.
	ModelScoped bool
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
	code, message := extractFailureMetadata(body)
	failure := Failure{Code: code, Message: message}
	lowerCode := strings.ToLower(code)
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
		failure.ModelScoped = isModelQuotaExhaustion(lowerCode + " " + lowerMessage)
	case status == 429:
		failure.Kind = FailureRateLimit
		failure.Reason = account.ReasonCooldown
		if failure.Code == "" {
			failure.Code = "rate_limit"
		}
	// Brand-new free accounts often 403 permission-denied on /responses for a
	// short window after mint even though /models + OIDC tokens are valid.
	// Treat as validating (retryable) so import does not quarantine them as auth.
	case isTransientChatDenied(status, lowerCode, lowerMessage):
		failure.Kind = FailureUpstream
		failure.Reason = account.ReasonValidating
		if failure.Code == "" {
			failure.Code = "permission-denied"
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

// extractFailureMetadata pulls code/message from flat Grok bodies and OpenAI-style
// nested {"error":{"code","message","type"}} envelopes.
func extractFailureMetadata(body []byte) (code, message string) {
	if len(body) == 0 {
		return "", ""
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return "", strings.TrimSpace(string(body))
	}
	if nested, ok := root["error"].(map[string]any); ok {
		code = firstStringField(nested, "code", "error_code")
		message = firstStringField(nested, "message", "error")
		if code == "" {
			code = firstStringField(root, "code", "error_code")
		}
		if message == "" {
			message = firstStringField(root, "message")
		}
		if typeName := firstStringField(nested, "type", "error_type"); code == "" && typeName != "" {
			code = typeName
		}
		return code, message
	}
	// Flat: {"code":"...","error":"string"} or {"message":"..."}.
	code = firstStringField(root, "code", "error_code")
	switch errVal := root["error"].(type) {
	case string:
		message = strings.TrimSpace(errVal)
	}
	if message == "" {
		message = firstStringField(root, "message")
	}
	if code == "" && message == "" {
		return "", strings.TrimSpace(string(body))
	}
	return code, message
}

func firstStringField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// ParseRetryAfter interprets an HTTP Retry-After header as a duration from now.
// Supports integer seconds and HTTP-date. Returns 0 when missing or unparsable.
func ParseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if now.IsZero() {
			now = time.Now().UTC()
		}
		d := when.UTC().Sub(now.UTC())
		if d <= 0 {
			return 0
		}
		return d
	}
	return 0
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
	if strings.Contains(code, "spending-limit") || strings.Contains(message, "spending-limit") {
		return true
	}
	if strings.Contains(code, "personal-team-blocked") {
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
		"used all the included free usage",
		"included free usage for model",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) || strings.Contains(code, marker) {
			return true
		}
	}
	return false
}

func isModelQuotaExhaustion(text string) bool {
	return strings.Contains(text, "used all the included free usage for model") ||
		strings.Contains(text, "included free usage for model")
}

// isTransientChatDenied reports provisioning-style chat denials that clear on
// their own. Hard credential failures (no auth context / invalid token) must
// NOT match here.
func isTransientChatDenied(status int, code, message string) bool {
	if status != 403 && status != 401 {
		return false
	}
	// Real credential death — keep as auth.
	hard := []string{
		"no auth context",
		"invalid or expired credentials",
		"invalid_token",
		"invalid token",
		"token has expired",
		"expired credentials",
	}
	combined := code + " " + message
	for _, marker := range hard {
		if strings.Contains(combined, marker) {
			return false
		}
	}
	if code == "permission-denied" || strings.Contains(code, "permission-denied") {
		return true
	}
	// Observed body: "Access to the chat endpoint is denied..."
	if strings.Contains(message, "chat endpoint is denied") ||
		strings.Contains(message, "access to the chat endpoint") {
		return true
	}
	return false
}

func isAuthFailure(status int, code, message string) bool {
	if isTransientChatDenied(status, code, message) {
		return false
	}
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
