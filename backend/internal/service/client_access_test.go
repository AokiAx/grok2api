package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type clientAccessRepository struct {
	required  bool
	items     map[[32]byte]clientkey.Credential
	lookup    [32]byte
	decision  repository.RateLimitDecision
	consumed  []string
	consumeAt []time.Time
}

func (r *clientAccessRepository) ClientAuthRequired(context.Context) (bool, error) {
	return r.required, nil
}

func (r *clientAccessRepository) FindClientKeyByHash(_ context.Context, hash [32]byte) (clientkey.Credential, bool, error) {
	r.lookup = hash
	item, found := r.items[hash]
	return item, found, nil
}

func (r *clientAccessRepository) ConsumeClientKeyRPM(_ context.Context, id string, at time.Time) (repository.RateLimitDecision, error) {
	r.consumed = append(r.consumed, id)
	r.consumeAt = append(r.consumeAt, at)
	return r.decision, nil
}

func accessCredential(t *testing.T, id, secret string, now time.Time, mutate func(*clientkey.ClientKey)) clientkey.Credential {
	t.Helper()
	key := clientkey.ClientKey{
		ID: id, Name: id, Origin: clientkey.OriginManaged, KeyHash: sha256.Sum256([]byte(secret)),
		KeyPrefix: "g2a_preview", ModelPolicy: clientkey.ModelPolicyAll,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if mutate != nil {
		mutate(&key)
	}
	credential, err := clientkey.NewCredential(key, nil)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	return credential
}

func TestClientAccessAuthenticatesByHashAndReturnsOpaquePrincipal(t *testing.T) {
	now := time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC)
	secret := "g2a_super_secret"
	credential := accessCredential(t, "ck_123", secret, now, nil)
	repo := &clientAccessRepository{required: true, items: map[[32]byte]clientkey.Credential{
		credential.Key.KeyHash: credential,
	}}
	access := NewClientAccess(repo, WithClientAccessClock(func() time.Time { return now }))

	grant, err := access.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if repo.lookup != sha256.Sum256([]byte(secret)) {
		t.Fatalf("lookup hash=%x", repo.lookup)
	}
	if grant.KeyID != "ck_123" || grant.Principal != "client-key:ck_123" || !grant.Authenticated {
		t.Fatalf("grant=%+v", grant)
	}
	if grant.Principal == secret || grant.Principal == "auth:"+secret {
		t.Fatal("principal retained plaintext credential")
	}
}

func TestClientAccessUnifiesMissingRevokedExpiredAndUnknownCredentials(t *testing.T) {
	now := time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC)
	active := accessCredential(t, "active", "active-secret", now, nil)
	revokedLegacy := accessCredential(t, "client-key-legacy-config", "legacy-secret", now, func(key *clientkey.ClientKey) {
		key.Origin = clientkey.OriginConfigAPIKey
		key.RevokedAt = now.Add(-time.Minute)
	})
	expired := accessCredential(t, "expired", "expired-secret", now, func(key *clientkey.ClientKey) {
		key.ExpiresAt = now
	})
	repo := &clientAccessRepository{required: true, items: map[[32]byte]clientkey.Credential{
		active.Key.KeyHash:        active,
		revokedLegacy.Key.KeyHash: revokedLegacy,
		expired.Key.KeyHash:       expired,
	}}
	access := NewClientAccess(repo, WithClientAccessClock(func() time.Time { return now }))

	for _, secret := range []string{"", "unknown", "legacy-secret", "expired-secret"} {
		grant, err := access.Authenticate(context.Background(), secret)
		if !errors.Is(err, ErrClientUnauthorized) {
			t.Fatalf("secret=%q grant=%+v err=%v want unified unauthorized", secret, grant, err)
		}
	}
}

func TestClientAccessAllowsAnonymousOnlyBeforeStickyAuthMarker(t *testing.T) {
	now := time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC)
	repo := &clientAccessRepository{required: false}
	access := NewClientAccess(repo, WithClientAccessClock(func() time.Time { return now }))
	grant, err := access.Authenticate(context.Background(), "")
	if err != nil || grant.Authenticated || grant.Principal != "" {
		t.Fatalf("pre-marker anonymous grant=%+v err=%v", grant, err)
	}
	if _, err := access.Authenticate(context.Background(), "present-but-unknown"); !errors.Is(err, ErrClientUnauthorized) {
		t.Fatalf("present unknown credential err=%v", err)
	}
}

func TestClientGrantAuthorizesEffectiveModelAfterApplyingDefault(t *testing.T) {
	grant := ClientGrant{
		Authenticated: true,
		KeyID:         "ck_limited",
		ModelPolicy:   clientkey.ModelPolicyAllowlist,
		ModelScopes:   []string{"grok-4.5", "grok-code-fast-1"},
	}
	model, err := grant.AuthorizeModel("", "grok-4.5")
	if err != nil || model != "grok-4.5" {
		t.Fatalf("default effective model=%q err=%v", model, err)
	}
	model, err = grant.AuthorizeModel(" GROK-CODE-FAST-1 ", "grok-4.5")
	if err != nil || model != "GROK-CODE-FAST-1" {
		t.Fatalf("requested effective model=%q err=%v", model, err)
	}
	if _, err := grant.AuthorizeModel("grok-3", "grok-4.5"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("unscoped model err=%v", err)
	}
	if _, err := grant.AuthorizeModel("", "grok-3"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("unscoped default err=%v", err)
	}
}

func TestClientGrantFiltersModelCatalogByScope(t *testing.T) {
	grant := ClientGrant{
		Authenticated: true,
		ModelPolicy:   clientkey.ModelPolicyAllowlist,
		ModelScopes:   []string{"grok-4.5", "GROK-CODE-FAST-1"},
	}
	got := grant.FilterModelIDs([]string{"grok-3", "grok-4.5", "grok-code-fast-1", "grok-4.5"})
	if len(got) != 3 || got[0] != "grok-4.5" || got[1] != "grok-code-fast-1" || got[2] != "grok-4.5" {
		t.Fatalf("filtered ids=%#v", got)
	}
	all := ClientGrant{Authenticated: true, ModelPolicy: clientkey.ModelPolicyAll}
	if got := all.FilterModelIDs([]string{"a", "b"}); len(got) != 2 {
		t.Fatalf("all-policy filter=%#v", got)
	}
}

func TestClientAccessConsumesRepositoryRPMWithoutCallerSuppliedLimit(t *testing.T) {
	now := time.Date(2026, 7, 15, 5, 30, 0, 0, time.UTC)
	reset := now.Truncate(time.Minute).Add(time.Minute)
	repo := &clientAccessRepository{decision: repository.RateLimitDecision{
		Allowed: true, Limit: 10, Remaining: 9, ResetAt: reset,
	}}
	access := NewClientAccess(repo, WithClientAccessClock(func() time.Time { return now }))
	grant := ClientGrant{Authenticated: true, KeyID: "ck_limited", RPMLimit: 9999}
	decision, err := access.ConsumeRPM(context.Background(), grant)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(repo.consumed) != 1 || repo.consumed[0] != "ck_limited" || !repo.consumeAt[0].Equal(now) {
		t.Fatalf("repository calls ids=%#v at=%#v", repo.consumed, repo.consumeAt)
	}
	if decision.Limit != 10 || decision.Remaining != 9 || !decision.ResetAt.Equal(reset) {
		t.Fatalf("decision=%+v", decision)
	}

	// Even when the grant says 0, the repository is still authoritative and
	// returns the unlimited decision atomically with current persisted policy.
	repo.decision = repository.RateLimitDecision{Allowed: true, Limit: 0, ResetAt: reset}
	grant.RPMLimit = 0
	decision, err = access.ConsumeRPM(context.Background(), grant)
	if err != nil || !decision.Allowed || decision.Limit != 0 || len(repo.consumed) != 2 {
		t.Fatalf("unlimited decision=%+v err=%v calls=%#v", decision, err, repo.consumed)
	}
}

func TestClientAccessReturnsRateLimitedDecision(t *testing.T) {
	now := time.Date(2026, 7, 15, 5, 30, 0, 0, time.UTC)
	repo := &clientAccessRepository{decision: repository.RateLimitDecision{
		Allowed: false, Limit: 2, Remaining: 0, ResetAt: now.Add(30 * time.Second),
	}}
	access := NewClientAccess(repo, WithClientAccessClock(func() time.Time { return now }))
	decision, err := access.ConsumeRPM(context.Background(), ClientGrant{Authenticated: true, KeyID: "ck_limited"})
	if !errors.Is(err, ErrClientRateLimited) || decision.Allowed || decision.Limit != 2 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}
