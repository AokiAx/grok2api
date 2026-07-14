// Package account is a temporary compatibility facade. New code should import
// internal/domain/account, which owns the real account domain definitions.
package account

import domain "github.com/AokiAx/grok2api/backend/internal/domain/account"

type Pool = domain.Pool

const (
	PoolReady       = domain.PoolReady
	PoolUnavailable = domain.PoolUnavailable
)

type UnavailableReason = domain.UnavailableReason

const (
	ReasonQuota      = domain.ReasonQuota
	ReasonAuth       = domain.ReasonAuth
	ReasonCooldown   = domain.ReasonCooldown
	ReasonValidating = domain.ReasonValidating
	ReasonDisabled   = domain.ReasonDisabled
)

type Account = domain.Account
