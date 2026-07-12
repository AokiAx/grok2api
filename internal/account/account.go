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
