// Package clientkeys implements managed client-key lifecycle operations. Raw
// secrets exist only while Create is returning and are never retained by the
// service or exposed by later reads.
package clientkeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

var (
	ErrNotFound = errors.New("client key not found")
	ErrInvalid  = errors.New("invalid client key")
	ErrRevoked  = errors.New("client key is revoked")
	ErrExpired  = errors.New("client key is expired")
)

type Store interface {
	CreateClientKey(context.Context, clientkey.Credential) error
	GetClientKey(context.Context, string) (clientkey.Credential, bool, error)
	ListClientKeysPage(context.Context, repository.ListClientKeysQuery) (repository.ListClientKeysResult, error)
	UpdateClientKeyPolicy(context.Context, string, repository.ClientKeyPolicyUpdate) error
	RevokeClientKey(context.Context, string, time.Time) error
}

type CreateRequest struct {
	Name          string
	ModelPolicy   clientkey.ModelPolicy
	Scopes        []string
	RPMLimit      int
	MaxConcurrent int
	ExpiresAt     time.Time
}

type UpdateRequest struct {
	Name          string
	ModelPolicy   clientkey.ModelPolicy
	Scopes        []string
	RPMLimit      int
	MaxConcurrent int
	ExpiresAt     time.Time
}

// Result is safe to serialize. Secret is populated only by Create. KeyHash is
// always cleared before a result leaves the service boundary.
type Result struct {
	Key    clientkey.ClientKey
	Scopes []string
	Secret string
}

type ListResult struct {
	Items    []Result
	Total    int
	Page     int
	PageSize int
}

type Service struct {
	store  Store
	now    func() time.Time
	random io.Reader
}

type Option func(*Service)

func WithClock(now func() time.Time) Option {
	return func(service *Service) {
		if now != nil {
			service.now = now
		}
	}
}

func WithRandom(random io.Reader) Option {
	return func(service *Service) {
		if random != nil {
			service.random = random
		}
	}
}

func NewService(store Store, options ...Option) *Service {
	service := &Service{store: store, now: time.Now, random: rand.Reader}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	at, err := s.currentTime()
	if err != nil {
		return Result{}, err
	}
	if !request.ExpiresAt.IsZero() && !request.ExpiresAt.After(at) {
		return Result{}, fmt.Errorf("%w: expiry must be in the future", ErrInvalid)
	}
	idBytes, secretBytes := make([]byte, 16), make([]byte, 32)
	if _, err := io.ReadFull(s.random, idBytes); err != nil {
		return Result{}, fmt.Errorf("generate client key id: %w", err)
	}
	if _, err := io.ReadFull(s.random, secretBytes); err != nil {
		return Result{}, fmt.Errorf("generate client key secret: %w", err)
	}
	id := "ck_" + base64.RawURLEncoding.EncodeToString(idBytes)
	secret := "g2a_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	hash := sha256.Sum256([]byte(secret))
	prefixLength := 12
	if len(secret) < prefixLength {
		prefixLength = len(secret)
	}
	credential, err := clientkey.NewCredential(clientkey.ClientKey{
		ID:            id,
		Name:          request.Name,
		Origin:        clientkey.OriginManaged,
		KeyHash:       hash,
		KeyPrefix:     secret[:prefixLength],
		ModelPolicy:   request.ModelPolicy,
		RPMLimit:      request.RPMLimit,
		MaxConcurrent: request.MaxConcurrent,
		ExpiresAt:     request.ExpiresAt,
		CreatedAt:     at,
		UpdatedAt:     at,
	}, request.Scopes)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.store.CreateClientKey(ctx, credential); err != nil {
		return Result{}, fmt.Errorf("create client key: %w", err)
	}
	result := safeResult(credential)
	result.Secret = secret
	return result, nil
}

func (s *Service) Get(ctx context.Context, id string) (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	credential, found, err := s.store.GetClientKey(ctx, strings.TrimSpace(id))
	if err != nil {
		return Result{}, fmt.Errorf("get client key: %w", err)
	}
	if !found {
		return Result{}, ErrNotFound
	}
	return safeResult(credential), nil
}

func (s *Service) List(ctx context.Context, query repository.ListClientKeysQuery) (ListResult, error) {
	if err := s.ready(); err != nil {
		return ListResult{}, err
	}
	page, err := s.store.ListClientKeysPage(ctx, query)
	if err != nil {
		return ListResult{}, fmt.Errorf("list client keys: %w", err)
	}
	items := make([]Result, 0, len(page.Items))
	for _, credential := range page.Items {
		items = append(items, safeResult(credential))
	}
	return ListResult{Items: items, Total: page.Total, Page: page.Page, PageSize: page.PageSize}, nil
}

func (s *Service) Update(ctx context.Context, id string, request UpdateRequest) (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	at, err := s.currentTime()
	if err != nil {
		return Result{}, err
	}
	stored, found, err := s.store.GetClientKey(ctx, strings.TrimSpace(id))
	if err != nil {
		return Result{}, fmt.Errorf("get client key for update: %w", err)
	}
	if !found {
		return Result{}, ErrNotFound
	}
	if !stored.Key.RevokedAt.IsZero() {
		return Result{}, ErrRevoked
	}
	if !stored.Key.ExpiresAt.IsZero() && !at.Before(stored.Key.ExpiresAt) {
		return Result{}, ErrExpired
	}
	if !request.ExpiresAt.IsZero() && !request.ExpiresAt.After(at) {
		return Result{}, fmt.Errorf("%w: expiry must be in the future", ErrInvalid)
	}
	candidate := stored.Key
	candidate.Name = request.Name
	candidate.ModelPolicy = request.ModelPolicy
	candidate.RPMLimit = request.RPMLimit
	candidate.MaxConcurrent = request.MaxConcurrent
	candidate.ExpiresAt = request.ExpiresAt
	candidate.UpdatedAt = at
	credential, err := clientkey.NewCredential(candidate, request.Scopes)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	update := repository.ClientKeyPolicyUpdate{
		Name: credential.Key.Name, ModelPolicy: credential.Key.ModelPolicy, Scopes: credential.Scopes(),
		RPMLimit: credential.Key.RPMLimit, MaxConcurrent: credential.Key.MaxConcurrent,
		ExpiresAt: credential.Key.ExpiresAt, UpdatedAt: at,
	}
	if err := s.store.UpdateClientKeyPolicy(ctx, credential.Key.ID, update); err != nil {
		return Result{}, fmt.Errorf("update client key: %w", err)
	}
	return safeResult(credential), nil
}

func (s *Service) Revoke(ctx context.Context, id string) (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	at, err := s.currentTime()
	if err != nil {
		return Result{}, err
	}
	stored, found, err := s.store.GetClientKey(ctx, strings.TrimSpace(id))
	if err != nil {
		return Result{}, fmt.Errorf("get client key for revocation: %w", err)
	}
	if !found {
		return Result{}, ErrNotFound
	}
	if !stored.Key.RevokedAt.IsZero() {
		return Result{}, ErrRevoked
	}
	if !stored.Key.ExpiresAt.IsZero() && !at.Before(stored.Key.ExpiresAt) {
		return Result{}, ErrExpired
	}
	if err := s.store.RevokeClientKey(ctx, stored.Key.ID, at); err != nil {
		return Result{}, fmt.Errorf("revoke client key: %w", err)
	}
	stored.Key.RevokedAt = at
	stored.Key.UpdatedAt = at
	return safeResult(stored), nil
}

func (s *Service) ready() error {
	if s == nil || s.store == nil || s.now == nil || s.random == nil {
		return errors.New("client key service dependencies are required")
	}
	return nil
}

func (s *Service) currentTime() (time.Time, error) {
	at := s.now()
	if at.IsZero() {
		return time.Time{}, errors.New("client key service clock returned zero time")
	}
	return at.UTC(), nil
}

func safeResult(credential clientkey.Credential) Result {
	key := credential.Key
	key.KeyHash = [32]byte{}
	return Result{Key: key, Scopes: credential.Scopes()}
}
