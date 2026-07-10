package upstream

import (
	"encoding/json"
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
