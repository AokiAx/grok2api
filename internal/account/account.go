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

func (a Account) Available(_ time.Time) bool {
	maxActive := a.MaxActive
	if maxActive <= 0 {
		maxActive = 1
	}
	return a.Pool == PoolReady && a.Active < maxActive
}
