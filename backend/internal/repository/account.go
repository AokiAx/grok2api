// Package repository defines persistence ports and transport-neutral DTOs used
// by the application layer. Concrete database implementations live under
// internal/infra/persistence.
package repository

import (
	"context"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
)

// AccountLister loads all persisted accounts.
type AccountLister interface {
	ListAccounts(context.Context) ([]account.Account, error)
}

// AccountSaver persists one account and its state transition.
type AccountSaver interface {
	SaveAccount(context.Context, account.Account) error
}

// AccountBatchSaver persists multiple accounts atomically.
type AccountBatchSaver interface {
	SaveAccounts(context.Context, []account.Account) error
}

// AccountStore is the read/write account port used by recovery workflows.
type AccountStore interface {
	AccountLister
	AccountSaver
}

// AccountPageLister provides filtered, paginated account reads.
type AccountPageLister interface {
	ListAccountsPage(context.Context, ListAccountsQuery) (ListAccountsResult, error)
}

// AccountStatsReader provides aggregate account statistics without token rows.
type AccountStatsReader interface {
	AccountStats(context.Context) (AccountStats, error)
}

// AccountDeleter removes an account from persistence.
type AccountDeleter interface {
	DeleteAccount(context.Context, string) error
}

// AccountBatchDeleter removes multiple accounts atomically.
type AccountBatchDeleter interface {
	DeleteAccounts(context.Context, []string) error
}

// AccountReader loads one account by stable ID.
type AccountReader interface {
	GetAccount(context.Context, string) (account.Account, bool, error)
}

// AccountEventReader exposes the administrative account timeline.
type AccountEventReader interface {
	ListAccountEvents(context.Context, ListAccountEventsQuery) (ListAccountEventsResult, error)
}

// AccountRepository is the complete account persistence port used by account
// administration. Narrower consumers should accept the focused interfaces
// above instead.
type AccountRepository interface {
	AccountStore
	AccountPageLister
	AccountStatsReader
	AccountDeleter
	AccountBatchSaver
	AccountBatchDeleter
	AccountReader
	AccountEventReader
}

type AccountEventType string

const (
	AccountEventStateTransition AccountEventType = "state_transition"
	AccountEventConfiguration   AccountEventType = "configuration"
	AccountEventDeletion        AccountEventType = "deletion"
)

// AccountEvent is one persisted account lifecycle or configuration change.
type AccountEvent struct {
	ID        int64
	AccountID string
	Type      AccountEventType
	FromPool  account.Pool
	ToPool    account.Pool
	Reason    string
	ErrorCode string
	CreatedAt time.Time
	Details   map[string]any
}

type ListAccountEventsQuery struct {
	AccountID string
	Page      int
	PageSize  int
}

type ListAccountEventsResult struct {
	Items    []AccountEvent
	Total    int
	Page     int
	PageSize int
}

// ListAccountsQuery filters and pages accounts for administration views.
// Page is 1-based. Implementations may normalize invalid values.
type ListAccountsQuery struct {
	Pool     string // "", "ready", or "unavailable"
	Q        string // substring match on id/email/reason/error
	Page     int
	PageSize int
}

// ListAccountsResult is one page of accounts plus the filtered total.
type ListAccountsResult struct {
	Items    []account.Account
	Total    int
	Page     int
	PageSize int
}

// AccountStats is a lightweight global aggregate that excludes token rows.
type AccountStats struct {
	TotalAccounts       int
	ReadyAccounts       int
	UnavailableAccounts int
	TotalRequests       int64
	MaxActive           int
	RefreshableAccounts int
	QuotaActual         int64
	QuotaLimit          int64
	QuotaRemaining      int64
	ReadyQuotaRemaining int64
	QuotaObserved       int
	ReadyQuotaObserved  int
	// AuthFailAccounts is the number of accounts with authentication failures.
	AuthFailAccounts int
	// TotalAuthFails is the sum of authentication failures across accounts.
	TotalAuthFails int64
	// AccessExpired counts non-empty expirations earlier than now.
	AccessExpired int
	// AccessExpiringSoon counts accounts expiring within the next hour.
	AccessExpiringSoon int
	// RetryDue counts unavailable accounts whose retry time is due.
	RetryDue int
	// NoRefreshToken counts accounts without a refresh token.
	NoRefreshToken int
	Reasons        map[string]int
	// ErrorCodes aggregates non-empty last error codes.
	ErrorCodes map[string]int
}
