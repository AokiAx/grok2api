package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

var (
	ErrClientUnauthorized = errors.New("client credential is invalid")
	ErrModelNotAllowed    = errors.New("model is not allowed for this client key")
)

type ClientAccessRepository interface {
	ClientAuthRequired(context.Context) (bool, error)
	FindClientKeyByHash(context.Context, [32]byte) (clientkey.Credential, bool, error)
	ConsumeClientKeyRPM(context.Context, string, time.Time) (repository.RateLimitDecision, error)
}

// ClientGrant is the safe request identity derived from a client key. It
// deliberately contains neither the raw secret nor its hash.
type ClientGrant struct {
	Authenticated bool
	KeyID         string
	Principal     string
	ModelPolicy   clientkey.ModelPolicy
	ModelScopes   []string
	RPMLimit      int
	MaxConcurrent int
}

func (g ClientGrant) AllowsModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if !g.Authenticated || g.ModelPolicy == clientkey.ModelPolicyAll {
		return true
	}
	if g.ModelPolicy != clientkey.ModelPolicyAllowlist {
		return false
	}
	for _, scope := range g.ModelScopes {
		if strings.ToLower(strings.TrimSpace(scope)) == model {
			return true
		}
	}
	return false
}

// AuthorizeModel computes the effective model first, then evaluates the key's
// policy. This prevents an omitted model from bypassing an allowlist through a
// server-side default.
func (g ClientGrant) AuthorizeModel(requested, defaultModel string) (string, error) {
	model := strings.TrimSpace(requested)
	if model == "" {
		model = strings.TrimSpace(defaultModel)
	}
	if !g.AllowsModel(model) {
		return "", ErrModelNotAllowed
	}
	return model, nil
}

func (g ClientGrant) FilterModelIDs(models []string) []string {
	if !g.Authenticated || g.ModelPolicy == clientkey.ModelPolicyAll {
		return append([]string(nil), models...)
	}
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if g.AllowsModel(model) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

type ClientAccess struct {
	repository ClientAccessRepository
	now        func() time.Time
}

type ClientAccessOption func(*ClientAccess)

func WithClientAccessClock(now func() time.Time) ClientAccessOption {
	return func(access *ClientAccess) {
		if now != nil {
			access.now = now
		}
	}
}

func NewClientAccess(repository ClientAccessRepository, options ...ClientAccessOption) *ClientAccess {
	access := &ClientAccess{repository: repository, now: time.Now}
	for _, option := range options {
		option(access)
	}
	return access
}

func (a *ClientAccess) Authenticate(ctx context.Context, secret string) (ClientGrant, error) {
	if a == nil || a.repository == nil || a.now == nil {
		return ClientGrant{}, errors.New("client access dependencies are required")
	}
	required, err := a.repository.ClientAuthRequired(ctx)
	if err != nil {
		return ClientGrant{}, fmt.Errorf("read client authentication policy: %w", err)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		if required {
			return ClientGrant{}, ErrClientUnauthorized
		}
		return ClientGrant{}, nil
	}
	hash := sha256.Sum256([]byte(secret))
	credential, found, err := a.repository.FindClientKeyByHash(ctx, hash)
	if err != nil {
		return ClientGrant{}, fmt.Errorf("look up client credential: %w", err)
	}
	if !found {
		return ClientGrant{}, ErrClientUnauthorized
	}
	at := a.now()
	if at.IsZero() {
		return ClientGrant{}, errors.New("client access clock returned zero time")
	}
	if !credential.Key.Active(at.UTC()) {
		return ClientGrant{}, ErrClientUnauthorized
	}
	return ClientGrant{
		Authenticated: true,
		KeyID:         credential.Key.ID,
		Principal:     "client-key:" + credential.Key.ID,
		ModelPolicy:   credential.Key.ModelPolicy,
		ModelScopes:   credential.Scopes(),
		RPMLimit:      credential.Key.RPMLimit,
		MaxConcurrent: credential.Key.MaxConcurrent,
	}, nil
}

type clientGrantContextKey struct{}

func WithClientGrant(ctx context.Context, grant ClientGrant) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clientGrantContextKey{}, grant)
}

func ClientGrantFromContext(ctx context.Context) (ClientGrant, bool) {
	if ctx == nil {
		return ClientGrant{}, false
	}
	grant, ok := ctx.Value(clientGrantContextKey{}).(ClientGrant)
	return grant, ok
}

type effectiveModelContextKey struct{}

func WithEffectiveModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, effectiveModelContextKey{}, strings.TrimSpace(model))
}

func EffectiveModelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	model, ok := ctx.Value(effectiveModelContextKey{}).(string)
	return model, ok && model != ""
}
