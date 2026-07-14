package repository

import (
	"context"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
)

type ClientKeyStore interface {
	CreateClientKey(context.Context, clientkey.ClientKey, []string) error
	GetClientKey(context.Context, string) (clientkey.ClientKey, []string, bool, error)
	FindClientKeyByHash(context.Context, [32]byte) (clientkey.ClientKey, []string, bool, error)
	UpdateClientKey(context.Context, clientkey.ClientKey, []string) error
	RevokeClientKey(context.Context, string, time.Time) error
}

type ClientAuthPolicyReader interface {
	ClientAuthRequired(context.Context) (bool, error)
}

type ClientKeyRateLimiter interface {
	ConsumeClientKeyRPM(context.Context, string, int, time.Time) (RateLimitDecision, error)
}

type ClientKeyRepository interface {
	ClientKeyStore
	ClientAuthPolicyReader
	ClientKeyRateLimiter
}

type RateLimitDecision struct {
	Allowed   bool
	Limit     int
	Remaining int
	ResetAt   time.Time
}
