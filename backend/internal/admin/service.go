package admin

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/account"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

// ListAccountsQuery is the admin-facing account list filter/page request.
type ListAccountsQuery struct {
	Pool     string // "", "ready", "unavailable"
	Q        string
	Page     int
	PageSize int
}

// ListAccountsPage is one filtered page of accounts.
type ListAccountsPage struct {
	Accounts []account.Account
	Total    int
	Page     int
	PageSize int
}

// AccountStats is a global pool aggregate for the panel summary.
type AccountStats struct {
	TotalAccounts       int            `json:"total_accounts"`
	ReadyAccounts       int            `json:"ready_accounts"`
	UnavailableAccounts int            `json:"unavailable_accounts"`
	ActiveLeases        int            `json:"active_leases"`
	MaxActive           int            `json:"max_active"`
	TotalRequests       int64          `json:"total_requests"`
	RefreshableAccounts int            `json:"refreshable_accounts"`
	QuotaActual         int64          `json:"quota_actual"`
	QuotaLimit          int64          `json:"quota_limit"`
	QuotaRemaining      int64          `json:"quota_remaining"`
	ReadyQuotaRemaining int64          `json:"ready_quota_remaining"`
	QuotaObserved       int            `json:"quota_observed_accounts"`
	ReadyQuotaObserved  int            `json:"ready_quota_observed_accounts"`
	AuthFailAccounts    int            `json:"auth_fail_accounts"`
	TotalAuthFails      int64          `json:"total_auth_fails"`
	AccessExpired       int            `json:"access_expired"`
	AccessExpiringSoon  int            `json:"access_expiring_soon"`
	RetryDue            int            `json:"retry_due"`
	NoRefreshToken      int            `json:"no_refresh_token"`
	Reasons             map[string]int `json:"reasons"`
	ErrorCodes          map[string]int `json:"error_codes"`
}

type Repository interface {
	ListAccounts(context.Context) ([]account.Account, error)
	ListAccountsPage(context.Context, repository.ListAccountsQuery) (repository.ListAccountsResult, error)
	AccountStats(context.Context) (repository.AccountStats, error)
	SaveAccount(context.Context, account.Account) error
	DeleteAccount(context.Context, string) error
}

type Validator interface {
	Validate(context.Context, account.Account) (account.UnavailableReason, string, error)
}

type AccountSink interface {
	Upsert(account.Account)
	Delete(string) bool
}

type Service struct {
	repository Repository
	validator  Validator
	sink       AccountSink
	now        func() time.Time
	quotaRetry time.Duration
	rateRetry  time.Duration
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
		quotaRetry: 24 * time.Hour,
		rateRetry:  45 * time.Second,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

var ErrAccountNotFound = errors.New("account not found")

type ImportAccount struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	AccessToken  string `json:"access_token"` // legacy alias for key
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	// ExpiresAt accepts ~/.grok/auth.json style absolute expiry (RFC3339 / RFC3339Nano).
	ExpiresAt    string `json:"expires_at"`
	Email        string `json:"email"`
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
	UserID       string `json:"user_id"`
	TeamID       string `json:"team_id"`
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

// ListPage returns one filtered page of accounts without loading the whole table.
func (s *Service) ListPage(ctx context.Context, query ListAccountsQuery) (ListAccountsPage, error) {
	result, err := s.repository.ListAccountsPage(ctx, repository.ListAccountsQuery{
		Pool:     query.Pool,
		Q:        query.Q,
		Page:     query.Page,
		PageSize: query.PageSize,
	})
	if err != nil {
		return ListAccountsPage{}, err
	}
	return ListAccountsPage{
		Accounts: result.Items,
		Total:    result.Total,
		Page:     result.Page,
		PageSize: result.PageSize,
	}, nil
}

// Stats returns global pool aggregates for the admin summary cards.
func (s *Service) Stats(ctx context.Context) (AccountStats, error) {
	raw, err := s.repository.AccountStats(ctx)
	if err != nil {
		return AccountStats{}, err
	}
	reasons := raw.Reasons
	if reasons == nil {
		reasons = map[string]int{}
	}
	errorCodes := raw.ErrorCodes
	if errorCodes == nil {
		errorCodes = map[string]int{}
	}
	return AccountStats{
		TotalAccounts:       raw.TotalAccounts,
		ReadyAccounts:       raw.ReadyAccounts,
		UnavailableAccounts: raw.UnavailableAccounts,
		MaxActive:           raw.MaxActive,
		TotalRequests:       raw.TotalRequests,
		RefreshableAccounts: raw.RefreshableAccounts,
		QuotaActual:         raw.QuotaActual,
		QuotaLimit:          raw.QuotaLimit,
		QuotaRemaining:      raw.QuotaRemaining,
		ReadyQuotaRemaining: raw.ReadyQuotaRemaining,
		QuotaObserved:       raw.QuotaObserved,
		ReadyQuotaObserved:  raw.ReadyQuotaObserved,
		AuthFailAccounts:    raw.AuthFailAccounts,
		TotalAuthFails:      raw.TotalAuthFails,
		AccessExpired:       raw.AccessExpired,
		AccessExpiringSoon:  raw.AccessExpiringSoon,
		RetryDue:            raw.RetryDue,
		NoRefreshToken:      raw.NoRefreshToken,
		Reasons:             reasons,
		ErrorCodes:          errorCodes,
	}, nil
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
		token := strings.TrimSpace(input.Key)
		if token == "" {
			token = strings.TrimSpace(input.AccessToken)
		}
		if token == "" {
			result.Invalid++
			result.Items = append(result.Items, ImportItem{
				Index:   index,
				Status:  "invalid",
				Message: "key or access_token required",
			})
			continue
		}
		email := strings.ToLower(strings.TrimSpace(input.Email))
		userID := strings.TrimSpace(input.UserID)
		// ~/.grok/auth.json map keys look like issuer::client_id::user_id.
		if userID == "" {
			userID = userIDFromAuthMapKey(input.ID)
		}
		key := identity(email, userID, token)
		previous, exists := byIdentity[key]
		now := s.now().UTC()
		id := chooseImportID(input.ID, email, userID, token, previous, exists)

		item := previous
		item.ID = id
		item.AccessToken = token
		item.RefreshToken = firstNonEmpty(input.RefreshToken, previous.RefreshToken)
		item.Email = firstNonEmpty(email, previous.Email)
		item.UserID = firstNonEmpty(userID, previous.UserID)
		item.TeamID = firstNonEmpty(input.TeamID, previous.TeamID)
		item.OIDCIssuer = firstNonEmpty(input.OIDCIssuer, previous.OIDCIssuer, "https://auth.x.ai")
		item.OIDCClientID = firstNonEmpty(input.OIDCClientID, previous.OIDCClientID)
		if expiresAt, ok := resolveExpiresAt(input, now); ok {
			item.ExpiresAt = expiresAt
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
			item.RetryAt = time.Time{}
		} else {
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = reason
			item.LastErrorCode = errorCode
			item.RetryAt = s.retryAt(reason, item.RetryAt, now)
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

func (s *Service) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("delete account: %w", ErrAccountNotFound)
	}
	accounts, err := s.repository.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list accounts before delete: %w", err)
	}
	found := false
	for _, item := range accounts {
		if item.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("delete account %s: %w", id, ErrAccountNotFound)
	}
	if err := s.repository.DeleteAccount(ctx, id); err != nil {
		return fmt.Errorf("delete account %s: %w", id, err)
	}
	if s.sink != nil {
		s.sink.Delete(id)
	}
	return nil
}

func (s *Service) Recover(ctx context.Context, id string) (account.Account, error) {
	id = strings.TrimSpace(id)
	accounts, err := s.repository.ListAccounts(ctx)
	if err != nil {
		return account.Account{}, fmt.Errorf("list accounts before recovery: %w", err)
	}
	var item account.Account
	found := false
	for _, candidate := range accounts {
		if candidate.ID == id {
			item = candidate
			found = true
			break
		}
	}
	if !found {
		return account.Account{}, fmt.Errorf("recover account %s: %w", id, ErrAccountNotFound)
	}

	reason, errorCode, err := s.validator.Validate(ctx, item)
	if err != nil {
		return account.Account{}, fmt.Errorf("validate account %s: %w", id, err)
	}
	now := s.now().UTC()
	item.UpdatedAt = now
	item.LastErrorCode = errorCode
	if reason == "" {
		item.Pool = account.PoolReady
		item.UnavailableReason = ""
		item.RetryAt = time.Time{}
		item.LastErrorCode = ""
	} else {
		item.Pool = account.PoolUnavailable
		item.UnavailableReason = reason
		item.RetryAt = s.retryAt(reason, item.RetryAt, now)
	}
	if err := s.repository.SaveAccount(ctx, item); err != nil {
		return account.Account{}, fmt.Errorf("save recovered account %s: %w", id, err)
	}
	if s.sink != nil {
		s.sink.Upsert(item)
	}
	return item, nil
}

func (s *Service) retryAt(
	reason account.UnavailableReason,
	previous time.Time,
	now time.Time,
) time.Time {
	if !previous.IsZero() && previous.After(now) {
		return previous
	}
	switch reason {
	case account.ReasonQuota:
		return now.Add(s.quotaRetry)
	case account.ReasonCooldown:
		return now.Add(s.rateRetry)
	case account.ReasonValidating:
		// New-account chat provisioning window; recovery re-probes soon.
		return now.Add(45 * time.Second)
	case account.ReasonAuth:
		return now.Add(5 * time.Minute)
	default:
		return time.Time{}
	}
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

func chooseImportID(rawID, email, userID, token string, previous account.Account, exists bool) string {
	if exists {
		return previous.ID
	}
	id := strings.TrimSpace(rawID)
	// Avoid using compound auth map keys (issuer::client::user) as stable IDs.
	if isAuthMapKey(id) {
		id = ""
	}
	if id != "" {
		return id
	}
	if userID != "" {
		return userID
	}
	if email != "" {
		return email
	}
	digest := sha256.Sum256([]byte(token))
	return fmt.Sprintf("account-%x", digest[:8])
}

func isAuthMapKey(value string) bool {
	parts := strings.Split(value, "::")
	return len(parts) == 3 && strings.Contains(parts[0], "://")
}

func userIDFromAuthMapKey(value string) string {
	parts := strings.Split(strings.TrimSpace(value), "::")
	if len(parts) != 3 {
		return ""
	}
	if !strings.Contains(parts[0], "://") {
		return ""
	}
	return strings.TrimSpace(parts[2])
}

func resolveExpiresAt(input ImportAccount, now time.Time) (time.Time, bool) {
	if input.ExpiresIn > 0 {
		return now.Add(time.Duration(input.ExpiresIn) * time.Second), true
	}
	raw := strings.TrimSpace(input.ExpiresAt)
	if raw == "" {
		return time.Time{}, false
	}
	// auth.json uses nanosecond RFC3339; also accept plain RFC3339.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
