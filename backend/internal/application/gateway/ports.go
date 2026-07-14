package gateway

import (
	"context"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

// AccountPool leases accounts and reports the state needed by application
// retry and circuit policy.
type AccountPool interface {
	Acquire(context.Context, string) (Lease, error)
	Snapshot() PoolSnapshot
}

// Lease is the application-facing account reservation contract.
type Lease interface {
	Account() account.Account
	MoveUnavailable(account.UnavailableReason, time.Time, string)
	RecordUsage(int64, int64, time.Time)
	Release()
}

// Provider executes a normalized request with one selected domain account.
// Provider adapters own endpoint, protocol, and raw response classification.
type Provider interface {
	Do(context.Context, account.Account, Request) (Response, error)
}

// AccountStore is the focused repository boundary used to persist account
// state transitions made by the gateway application.
type AccountStore interface {
	repository.AccountSaver
}
