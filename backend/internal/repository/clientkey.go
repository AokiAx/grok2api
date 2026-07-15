package repository

import (
	"context"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
)

type ClientKeyStore interface {
	CreateClientKey(context.Context, clientkey.Credential) error
	GetClientKey(context.Context, string) (clientkey.Credential, bool, error)
	FindClientKeyByHash(context.Context, [32]byte) (clientkey.Credential, bool, error)
	ListClientKeysPage(context.Context, ListClientKeysQuery) (ListClientKeysResult, error)
	UpdateClientKeyPolicy(context.Context, string, ClientKeyPolicyUpdate) error
	UpdateClientKeyLastUsedAt(context.Context, string, time.Time) error
	RevokeClientKey(context.Context, string, time.Time) error
}

type ClientAuthPolicyReader interface {
	ClientAuthRequired(context.Context) (bool, error)
}

type ClientKeyRateLimiter interface {
	ConsumeClientKeyRPM(context.Context, string, time.Time) (RateLimitDecision, error)
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

type ListClientKeysQuery struct {
	Q        string
	Origin   clientkey.Origin
	Page     int
	PageSize int
}

type ListClientKeysResult struct {
	Items    []clientkey.Credential
	Total    int
	Page     int
	PageSize int
}

// ClientKeyPolicyUpdate deliberately excludes immutable identity, origin,
// hash, and revocation fields so policy changes cannot resurrect revoked keys
// or silently rotate credentials.
type ClientKeyPolicyUpdate struct {
	Name          string
	ModelPolicy   clientkey.ModelPolicy
	Scopes        []string
	RPMLimit      int
	MaxConcurrent int
	ExpiresAt     time.Time
	UpdatedAt     time.Time
}
