package admin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
)

type Repository interface {
	ListAccounts(context.Context) ([]account.Account, error)
	SaveAccount(context.Context, account.Account) error
}

type Validator interface {
	Validate(context.Context, account.Account) (account.UnavailableReason, string, error)
}

type AccountSink interface {
	Upsert(account.Account)
}

type Service struct {
	repository Repository
	validator  Validator
	sink       AccountSink
	now        func() time.Time
}

type Option func(*Service)

func WithSink(sink AccountSink) Option {
	return func(service *Service) {
		service.sink = sink
	}
}

func NewService(repository Repository, validator Validator, options ...Option) *Service {
	service := &Service{
		repository: repository,
		validator:  validator,
		now:        time.Now,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

type ImportAccount struct {
	ID           string `json:"id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Email        string `json:"email"`
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
	UserID       string `json:"user_id"`
}

type ImportRequest struct {
	Accounts []ImportAccount `json:"accounts"`
	DryRun   bool            `json:"dry_run"`
}

type ImportItem struct {
	Index     int    `json:"index"`
	Status    string `json:"status"`
	AccountID string `json:"account_id,omitempty"`
	Message   string `json:"message,omitempty"`
}

type ImportResult struct {
	Added   int          `json:"added"`
	Updated int          `json:"updated"`
	Invalid int          `json:"invalid"`
	Applied bool         `json:"applied"`
	Items   []ImportItem `json:"items"`
}

func (s *Service) List(ctx context.Context) ([]account.Account, error) {
	return s.repository.ListAccounts(ctx)
}

func (s *Service) Import(ctx context.Context, request ImportRequest) (ImportResult, error) {
	existing, err := s.repository.ListAccounts(ctx)
	if err != nil {
		return ImportResult{}, fmt.Errorf("list accounts before import: %w", err)
	}
	byIdentity := make(map[string]account.Account, len(existing))
	for _, item := range existing {
		byIdentity[identity(item.Email, item.UserID, item.AccessToken)] = item
	}

	result := ImportResult{Applied: !request.DryRun}
	for index, input := range request.Accounts {
		token := strings.TrimSpace(input.AccessToken)
		if token == "" {
			result.Invalid++
			result.Items = append(result.Items, ImportItem{
				Index:   index,
				Status:  "invalid",
				Message: "access_token required",
			})
			continue
		}
		email := strings.ToLower(strings.TrimSpace(input.Email))
		key := identity(email, input.UserID, token)
		previous, exists := byIdentity[key]
		now := s.now().UTC()
		id := strings.TrimSpace(input.ID)
		if exists {
			id = previous.ID
		} else if id == "" && email != "" {
			id = email
		} else if id == "" {
			digest := sha256.Sum256([]byte(token))
			id = fmt.Sprintf("account-%x", digest[:8])
		}

		item := previous
		item.ID = id
		item.AccessToken = token
		item.RefreshToken = firstNonEmpty(input.RefreshToken, previous.RefreshToken)
		item.Email = firstNonEmpty(email, previous.Email)
		item.UserID = firstNonEmpty(input.UserID, previous.UserID)
		item.OIDCIssuer = firstNonEmpty(input.OIDCIssuer, previous.OIDCIssuer, "https://auth.x.ai")
		item.OIDCClientID = firstNonEmpty(input.OIDCClientID, previous.OIDCClientID)
		if input.ExpiresIn > 0 {
			item.ExpiresAt = now.Add(time.Duration(input.ExpiresIn) * time.Second)
		}
		if item.MaxActive <= 0 {
			item.MaxActive = 1
		}
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		item.UpdatedAt = now

		status := "added"
		if exists {
			status = "updated"
			result.Updated++
		} else {
			result.Added++
		}
		result.Items = append(result.Items, ImportItem{
			Index:     index,
			Status:    status,
			AccountID: id,
		})
		if request.DryRun {
			continue
		}

		reason, errorCode, err := s.validator.Validate(ctx, item)
		if err != nil {
			return ImportResult{}, fmt.Errorf("validate account %s: %w", id, err)
		}
		if reason == "" {
			item.Pool = account.PoolReady
			item.UnavailableReason = ""
			item.LastErrorCode = ""
		} else {
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = reason
			item.LastErrorCode = errorCode
		}
		if err := s.repository.SaveAccount(ctx, item); err != nil {
			return ImportResult{}, fmt.Errorf("save imported account %s: %w", id, err)
		}
		if s.sink != nil {
			s.sink.Upsert(item)
		}
		byIdentity[key] = item
	}
	return result, nil
}

func identity(email, userID, token string) string {
	if userID = strings.TrimSpace(userID); userID != "" {
		return "user:" + userID
	}
	if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
		return "email:" + email
	}
	digest := sha256.Sum256([]byte(token))
	return fmt.Sprintf("token:%x", digest[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
