package gateway

import (
	"errors"
	"time"
)

// ErrNoReadyAccount supports callers that only need to distinguish account
// pool availability from other failures.
var ErrNoReadyAccount = errors.New("no ready account")

// SelectionReason explains why an account pool could not provide a lease or
// why application circuit policy rejected selection.
type SelectionReason string

const (
	SelectionNoReady      SelectionReason = "no_ready"
	SelectionSaturated    SelectionReason = "saturated"
	SelectionQuota        SelectionReason = "quota"
	SelectionCooling      SelectionReason = "cooling"
	SelectionAuth         SelectionReason = "auth"
	SelectionValidating   SelectionReason = "validating"
	SelectionQuotaCircuit SelectionReason = "quota_circuit"
)

// SelectionError is returned by AccountPool when no account can be leased.
type SelectionError struct {
	Reason      SelectionReason
	RetryAfter  time.Duration
	Ready       int
	Unavailable int
}

func (e *SelectionError) Error() string {
	if e == nil {
		return ErrNoReadyAccount.Error()
	}
	return selectionMessage(e.Reason, ErrNoReadyAccount.Error())
}

// Is preserves the sentinel semantics of account selection failures.
func (e *SelectionError) Is(target error) bool {
	return target == ErrNoReadyAccount
}

// AsSelectionError unwraps a SelectionError.
func AsSelectionError(err error) (*SelectionError, bool) {
	var target *SelectionError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// PoolUnavailableError reports application-level pool unavailability without
// embedding an HTTP status. Transports own protocol-specific status mapping.
type PoolUnavailableError struct {
	RetryAfter time.Duration
	Reason     SelectionReason
	Message    string
}

func (e *PoolUnavailableError) Error() string {
	if e == nil {
		return "ready account pool is empty"
	}
	if e.Message != "" {
		return e.Message
	}
	switch e.Reason {
	case SelectionQuota:
		return "no ready accounts (quota exhausted)"
	case SelectionCooling:
		return "no ready accounts (cooling down)"
	case SelectionAuth:
		return "no ready accounts (auth failures)"
	case SelectionValidating:
		return "no ready accounts (validating)"
	default:
		return selectionMessage(e.Reason, "ready account pool is empty")
	}
}

// Is preserves the sentinel semantics of application pool failures.
func (e *PoolUnavailableError) Is(target error) bool {
	return target == ErrNoReadyAccount
}

// AsPoolUnavailable unwraps a PoolUnavailableError.
func AsPoolUnavailable(err error) (*PoolUnavailableError, bool) {
	var target *PoolUnavailableError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func selectionMessage(reason SelectionReason, fallback string) string {
	switch reason {
	case SelectionSaturated:
		return "ready accounts are at concurrency capacity"
	case SelectionQuota:
		return "ready account pool exhausted by quota"
	case SelectionCooling:
		return "ready account pool cooling down"
	case SelectionAuth:
		return "ready account pool blocked by auth failures"
	case SelectionValidating:
		return "ready account pool validating"
	case SelectionQuotaCircuit:
		return "quota circuit open; retry later"
	default:
		return fallback
	}
}
