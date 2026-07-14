// Package gateway defines transport-neutral application contracts for account
// selection and provider execution.
package gateway

import (
	"io"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
)

// Operation identifies an application capability without exposing a provider
// endpoint or HTTP method.
type Operation string

const (
	OperationResponses Operation = "responses"
	OperationChat      Operation = "chat"
	OperationModels    Operation = "models"
	OperationBilling   Operation = "billing"
)

// Request is the provider-neutral input accepted by the gateway application.
// ConversationID and AffinityKey are explicit so provider and pool adapters do
// not need private context values.
type Request struct {
	Operation      Operation
	Body           []byte
	Stream         bool
	ConversationID string
	AffinityKey    string
}

// Header carries provider response metadata without importing a transport
// package into the application layer.
type Header map[string][]string

// Response is the provider-neutral result returned by the gateway.
type Response struct {
	Status  int
	Header  Header
	Body    []byte
	Stream  io.ReadCloser
	Failure *Failure
	Usage   Usage
}

// UsageUnit identifies which free-tier counter a provider observed.
type UsageUnit string

const (
	UsageTokens   UsageUnit = "tokens"
	UsageRequests UsageUnit = "requests"
)

// Usage is a normalized provider capacity observation.
type Usage struct {
	Present   bool
	Unit      UsageUnit
	Consumed  int64
	Limit     int64
	Remaining int64
}

// FailureKind is a provider-independent failure classification. Application
// policy maps these values to account state transitions and retry behavior.
type FailureKind string

const (
	FailureQuota             FailureKind = "quota"
	FailureRateLimit         FailureKind = "rate_limit"
	FailureAuthentication    FailureKind = "auth"
	FailureCredentialPending FailureKind = "credential_pending"
	FailureRequest           FailureKind = "request"
	FailureProvider          FailureKind = "provider"

	// FailureAuth is the concise name used by callers that already use auth as
	// their domain vocabulary.
	FailureAuth = FailureAuthentication
)

// Failure carries normalized provider failure data. It deliberately does not
// expose account.UnavailableReason; application policy owns that mapping.
type Failure struct {
	Kind       FailureKind
	Code       string
	Message    string
	RetryAfter time.Duration
	Usage      Usage
}

// PoolSnapshot is an atomic view of the account pool used for selection,
// retry, and circuit decisions.
type PoolSnapshot struct {
	Ready         int
	Unavailable   int
	Reasons       map[account.UnavailableReason]int
	EarliestRetry time.Time
	Revision      uint64
}
