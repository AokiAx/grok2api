package account

import "time"

type Pool string

const (
	PoolReady       Pool = "ready"
	PoolUnavailable Pool = "unavailable"
)

type UnavailableReason string

const (
	ReasonQuota      UnavailableReason = "quota"
	ReasonAuth       UnavailableReason = "auth"
	ReasonCooldown   UnavailableReason = "cooldown"
	ReasonValidating UnavailableReason = "validating"
	ReasonDisabled   UnavailableReason = "disabled"
)

const revokedRetry = 365 * 24 * time.Hour

type Account struct {
	ID                  string
	AccessToken         string
	RefreshToken        string
	ExpiresAt           time.Time
	OIDCIssuer          string
	OIDCClientID        string
	Email               string
	UserID              string
	TeamID              string
	Pool                Pool
	UnavailableReason   UnavailableReason
	RetryAt             time.Time
	LastErrorCode       string
	LastSuccessAt       time.Time
	QuotaActual         int64
	QuotaLimit          int64
	RequestCount        int64
	AuthenticationFails int
	Active              int
	MaxActive           int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// QuotaExhausted reports whether free-tier counters already show no remaining
// capacity. Used to keep known-empty accounts out of the ready scheduler.
func (a Account) QuotaExhausted() bool {
	return a.QuotaLimit > 0 && a.QuotaActual >= a.QuotaLimit
}

func (a Account) Available(_ time.Time) bool {
	maxActive := a.MaxActive
	if maxActive <= 0 {
		maxActive = 1
	}
	return a.Pool == PoolReady && a.Active < maxActive && !a.QuotaExhausted()
}

// MarkUnavailable records a complete unavailable transition at an explicit
// clock instant. Callers retain ownership of scheduling policy and error
// classification; the domain owns the atomic state mutation.
func (a *Account) MarkUnavailable(
	reason UnavailableReason,
	retryAt time.Time,
	errorCode string,
	at time.Time,
) {
	a.Pool = PoolUnavailable
	a.UnavailableReason = reason
	a.RetryAt = retryAt
	a.LastErrorCode = errorCode
	a.UpdatedAt = normalizeTime(at)
}

// MarkReady clears unavailable metadata without changing quota or auth-failure
// counters. This is the behavior required by cooldown promotion, admin
// validation, and successful proactive token refresh.
func (a *Account) MarkReady(at time.Time) {
	a.Pool = PoolReady
	a.UnavailableReason = ""
	a.RetryAt = time.Time{}
	a.LastErrorCode = ""
	a.UpdatedAt = normalizeTime(at)
}

// RecoverValidated marks a credential healthy after validation and clears the
// accumulated authentication-failure count.
func (a *Account) RecoverValidated(at time.Time) {
	a.MarkReady(at)
	a.AuthenticationFails = 0
}

// RecoverQuotaWindow returns an account to ready after its rolling quota
// window elapsed. The limit remains known while actual usage starts a new
// window at zero.
func (a *Account) RecoverQuotaWindow(at time.Time) {
	a.MarkReady(at)
	a.QuotaActual = 0
}

// ParkKnownExhausted moves a ready account with exhausted stored quota out of
// service. Existing diagnostic codes and future retry deadlines are retained.
func (a *Account) ParkKnownExhausted(now time.Time, retryAfter time.Duration) bool {
	if a.Pool != PoolReady || !a.QuotaExhausted() {
		return false
	}
	if retryAfter <= 0 {
		retryAfter = 24 * time.Hour
	}
	now = normalizeTime(now)
	a.Pool = PoolUnavailable
	a.UnavailableReason = ReasonQuota
	if a.LastErrorCode == "" {
		a.LastErrorCode = "local:quota-exhausted"
	}
	defaultRetry := now.Add(retryAfter)
	if a.RetryAt.IsZero() || a.RetryAt.Before(now) {
		a.RetryAt = defaultRetry
	}
	a.UpdatedAt = now
	return true
}

// RecoverCooldown promotes only a due cooldown account. A zero retry time is
// intentionally not due because it lacks an explicit recovery deadline.
func (a *Account) RecoverCooldown(now time.Time) bool {
	if a.Pool != PoolUnavailable || a.UnavailableReason != ReasonCooldown ||
		a.RetryAt.IsZero() || a.RetryAt.After(now) {
		return false
	}
	a.MarkReady(now)
	return true
}

// SetQuota records quota counters without marking a successful request.
func (a *Account) SetQuota(actual, limit int64) {
	a.QuotaActual = actual
	a.QuotaLimit = limit
}

// RecordUsage records quota counters and the successful observation time.
func (a *Account) RecordUsage(actual, limit int64, at time.Time) {
	at = normalizeTime(at)
	a.SetQuota(actual, limit)
	a.LastSuccessAt = at
	a.UpdatedAt = at
}

// DisableRevoked quarantines a permanently unusable credential. The supplied
// permanent error code is preserved; otherwise the stable fallback is used.
func (a *Account) DisableRevoked(at time.Time, permanentErrorCode string) {
	if permanentErrorCode == "" {
		permanentErrorCode = "refresh-revoked"
	}
	a.MarkUnavailable(ReasonDisabled, normalizeTime(at).Add(revokedRetry), permanentErrorCode, at)
}

// ApplyRefreshFailure records a failed token refresh, including its auth-fail
// increment. Permanent failures are quarantined; transient failures re-enter
// the auth recovery queue.
func (a *Account) ApplyRefreshFailure(permanent bool, at time.Time, transientBackoff time.Duration) {
	a.AuthenticationFails++
	if permanent {
		a.DisableRevoked(at, "")
		return
	}
	a.MarkUnavailable(ReasonAuth, normalizeTime(at).Add(transientBackoff), "refresh-failed", at)
}

// RecordValidationFailure increments authentication failures before callers
// evaluate escalation thresholds. It deliberately leaves pool/reason intact
// so transport failures can retain their validating state.
func (a *Account) RecordValidationFailure(errorCode string, retryAt, at time.Time) int {
	a.AuthenticationFails++
	a.LastErrorCode = errorCode
	a.RetryAt = retryAt
	a.UpdatedAt = normalizeTime(at)
	return a.AuthenticationFails
}

func normalizeTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at.UTC()
}
