package clientkeys

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type memoryStore struct {
	items       map[string]clientkey.Credential
	created     clientkey.Credential
	updates     []repository.ClientKeyPolicyUpdate
	revocations []time.Time
}

func (s *memoryStore) CreateClientKey(_ context.Context, credential clientkey.Credential) error {
	if s.items == nil {
		s.items = make(map[string]clientkey.Credential)
	}
	s.created = credential
	s.items[credential.Key.ID] = credential
	return nil
}

func (s *memoryStore) GetClientKey(_ context.Context, id string) (clientkey.Credential, bool, error) {
	item, ok := s.items[id]
	return item, ok, nil
}

func (s *memoryStore) FindClientKeyByHash(_ context.Context, hash [32]byte) (clientkey.Credential, bool, error) {
	for _, item := range s.items {
		if item.Key.KeyHash == hash {
			return item, true, nil
		}
	}
	return clientkey.Credential{}, false, nil
}

func (s *memoryStore) ListClientKeysPage(_ context.Context, query repository.ListClientKeysQuery) (repository.ListClientKeysResult, error) {
	items := make([]clientkey.Credential, 0, len(s.items))
	for _, item := range s.items {
		items = append(items, item)
	}
	return repository.ListClientKeysResult{Items: items, Total: len(items), Page: query.Page, PageSize: query.PageSize}, nil
}

func (s *memoryStore) UpdateClientKeyPolicy(_ context.Context, id string, update repository.ClientKeyPolicyUpdate) error {
	item, ok := s.items[id]
	if !ok {
		return errors.New("not found")
	}
	item.Key.Name = update.Name
	item.Key.ModelPolicy = update.ModelPolicy
	item.Key.RPMLimit = update.RPMLimit
	item.Key.MaxConcurrent = update.MaxConcurrent
	item.Key.ExpiresAt = update.ExpiresAt
	item.Key.UpdatedAt = update.UpdatedAt
	updated, err := clientkey.NewCredential(item.Key, update.Scopes)
	if err != nil {
		return err
	}
	s.items[id] = updated
	s.updates = append(s.updates, update)
	return nil
}

func (s *memoryStore) RevokeClientKey(_ context.Context, id string, at time.Time) error {
	item, ok := s.items[id]
	if !ok {
		return errors.New("not found")
	}
	if err := item.Key.Revoke(at); err != nil {
		return err
	}
	updated, err := clientkey.NewCredential(item.Key, item.Scopes())
	if err != nil {
		return err
	}
	s.items[id] = updated
	s.revocations = append(s.revocations, at)
	return nil
}

func TestServiceCreateGeneratesOneTimeHighEntropySecretAndStoresOnlyHash(t *testing.T) {
	now := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	store := &memoryStore{}
	random := bytes.NewReader(bytes.Repeat([]byte{0x5a}, 64))
	service := NewService(store, WithClock(func() time.Time { return now }), WithRandom(random))

	created, err := service.Create(context.Background(), CreateRequest{
		Name:          " Production ",
		ModelPolicy:   clientkey.ModelPolicyAllowlist,
		Scopes:        []string{" GROK-4.5 ", "grok-4.5", "grok-code-fast-1"},
		RPMLimit:      60,
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(created.Secret) < 40 || created.Secret[:4] != "g2a_" {
		t.Fatalf("secret is not a high-entropy managed credential: %q", created.Secret)
	}
	if created.Key.ID == "" || created.Key.Name != "Production" || created.Key.Origin != clientkey.OriginManaged {
		t.Fatalf("created key = %+v", created.Key)
	}
	if created.Key.KeyPrefix == "" || created.Key.KeyPrefix != store.created.Key.KeyPrefix {
		t.Fatalf("prefix mismatch: view=%q stored=%q", created.Key.KeyPrefix, store.created.Key.KeyPrefix)
	}
	if created.Key.ModelPolicy != clientkey.ModelPolicyAllowlist || len(created.Scopes) != 2 {
		t.Fatalf("policy aggregation = %+v scopes=%#v", created.Key, created.Scopes)
	}
	wantHash := sha256.Sum256([]byte(created.Secret))
	if store.created.Key.KeyHash != wantHash {
		t.Fatalf("stored hash = %x want %x", store.created.Key.KeyHash, wantHash)
	}
	if bytes.Contains([]byte(store.created.Key.KeyPrefix), []byte(created.Secret)) {
		t.Fatal("stored prefix contains the full secret")
	}

	got, err := service.Get(context.Background(), created.Key.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Secret != "" || got.Key.KeyHash != ([32]byte{}) {
		t.Fatalf("subsequent read disclosed credential material: %+v", got)
	}
}

func TestServiceLifecycleNeverResurrectsRevokedOrExpiredKeys(t *testing.T) {
	now := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	store := &memoryStore{}
	service := NewService(store, WithClock(func() time.Time { return now }), WithRandom(bytes.NewReader(bytes.Repeat([]byte{0x31}, 128))))
	created, err := service.Create(context.Background(), CreateRequest{
		Name: "short-lived", ModelPolicy: clientkey.ModelPolicyAll, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := service.Revoke(context.Background(), created.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := service.Revoke(context.Background(), created.Key.ID); !errors.Is(err, ErrRevoked) {
		t.Fatalf("second revoke err=%v want ErrRevoked", err)
	}
	if _, err := service.Update(context.Background(), created.Key.ID, UpdateRequest{
		Name: "resurrect", ModelPolicy: clientkey.ModelPolicyAll,
	}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("update revoked err=%v want ErrRevoked", err)
	}

	expiredStore := &memoryStore{items: map[string]clientkey.Credential{}}
	hash := sha256.Sum256([]byte("legacy-secret"))
	legacy, err := clientkey.NewCredential(clientkey.ClientKey{
		ID: "client-key-legacy-config", Name: "legacy", Origin: clientkey.OriginConfigAPIKey,
		KeyHash: hash, KeyPrefix: "legacy_1234", ModelPolicy: clientkey.ModelPolicyAll,
		ExpiresAt: now, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}, nil)
	if err != nil {
		t.Fatalf("legacy fixture: %v", err)
	}
	expiredStore.items[legacy.Key.ID] = legacy
	expiredService := NewService(expiredStore, WithClock(func() time.Time { return now }))
	if _, err := expiredService.Update(context.Background(), legacy.Key.ID, UpdateRequest{
		Name: "legacy", ModelPolicy: clientkey.ModelPolicyAll, ExpiresAt: now.Add(time.Hour),
	}); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired legacy update err=%v want ErrExpired", err)
	}
	if len(expiredStore.updates) != 0 {
		t.Fatal("expired legacy key was persisted as active policy")
	}
}

func TestServiceUpdateKeepsImmutableCredentialFields(t *testing.T) {
	now := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	store := &memoryStore{}
	service := NewService(store, WithClock(func() time.Time { return now }), WithRandom(bytes.NewReader(bytes.Repeat([]byte{0x42}, 128))))
	created, err := service.Create(context.Background(), CreateRequest{Name: "key", ModelPolicy: clientkey.ModelPolicyAll})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	originalHash := store.created.Key.KeyHash
	originalOrigin := store.created.Key.Origin
	originalPrefix := store.created.Key.KeyPrefix

	updated, err := service.Update(context.Background(), created.Key.ID, UpdateRequest{
		Name: "updated", ModelPolicy: clientkey.ModelPolicyAllowlist, Scopes: []string{"grok-4.5"},
		RPMLimit: 10, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	stored := store.items[created.Key.ID]
	if stored.Key.KeyHash != originalHash || stored.Key.Origin != originalOrigin || stored.Key.KeyPrefix != originalPrefix {
		t.Fatalf("immutable fields changed: %+v", stored.Key)
	}
	if updated.Secret != "" || updated.Key.KeyHash != ([32]byte{}) || updated.Key.Name != "updated" {
		t.Fatalf("unsafe update response: %+v", updated)
	}
}
