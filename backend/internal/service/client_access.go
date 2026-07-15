package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

var (
	ErrClientUnauthorized       = errors.New("client credential is invalid")
	ErrModelNotAllowed          = errors.New("model is not allowed for this client key")
	ErrClientRateLimited        = errors.New("client key request rate exceeded")
	ErrClientConcurrencyLimited = errors.New("client key concurrency exceeded")
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
	repository    ClientAccessRepository
	now           func() time.Time
	concurrencyMu sync.Mutex
	activeByKey   map[string]int
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
	access := &ClientAccess{repository: repository, now: time.Now, activeByKey: make(map[string]int)}
	for _, option := range options {
		option(access)
	}
	return access
}

type ClientPermit interface {
	Release()
}

type clientPermit struct {
	access *ClientAccess
	keyID  string
	once   sync.Once
}

func (p *clientPermit) Release() {
	if p == nil || p.access == nil || p.keyID == "" {
		return
	}
	p.once.Do(func() {
		p.access.concurrencyMu.Lock()
		defer p.access.concurrencyMu.Unlock()
		active := p.access.activeByKey[p.keyID]
		if active <= 1 {
			delete(p.access.activeByKey, p.keyID)
			return
		}
		p.access.activeByKey[p.keyID] = active - 1
	})
}

type unlimitedClientPermit struct{}

func (unlimitedClientPermit) Release() {}

// AcquireConcurrency reserves one in-memory slot for the authenticated key.
// A zero limit is explicitly unlimited and returns a no-op permit.
func (a *ClientAccess) AcquireConcurrency(grant ClientGrant) (ClientPermit, error) {
	if !grant.Authenticated || grant.MaxConcurrent == 0 {
		return unlimitedClientPermit{}, nil
	}
	if grant.KeyID == "" || grant.MaxConcurrent < 0 {
		return nil, ErrClientUnauthorized
	}
	if a == nil {
		return nil, errors.New("client access is required")
	}
	a.concurrencyMu.Lock()
	defer a.concurrencyMu.Unlock()
	if a.activeByKey == nil {
		a.activeByKey = make(map[string]int)
	}
	if a.activeByKey[grant.KeyID] >= grant.MaxConcurrent {
		return nil, ErrClientConcurrencyLimited
	}
	a.activeByKey[grant.KeyID]++
	return &clientPermit{access: a, keyID: grant.KeyID}, nil
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

// ConsumeRPM delegates the whole decision to the repository so the persisted
// policy and counter are consumed atomically. The grant's copied RPMLimit is
// intentionally not passed back as caller-controlled input.
func (a *ClientAccess) ConsumeRPM(ctx context.Context, grant ClientGrant) (repository.RateLimitDecision, error) {
	if !grant.Authenticated {
		return repository.RateLimitDecision{Allowed: true, Limit: 0}, nil
	}
	if a == nil || a.repository == nil || a.now == nil {
		return repository.RateLimitDecision{}, errors.New("client access dependencies are required")
	}
	at := a.now()
	if at.IsZero() {
		return repository.RateLimitDecision{}, errors.New("client access clock returned zero time")
	}
	decision, err := a.repository.ConsumeClientKeyRPM(ctx, grant.KeyID, at.UTC())
	if err != nil {
		return repository.RateLimitDecision{}, fmt.Errorf("consume client key rpm: %w", err)
	}
	if !decision.Allowed {
		return decision, ErrClientRateLimited
	}
	return decision, nil
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
