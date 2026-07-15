package admin

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
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

type Validator interface {
	Validate(context.Context, account.Account) (account.UnavailableReason, string, error)
}

// Maintenance performs explicit, operator-triggered upstream account checks.
// It is injected separately from Validator so administrative actions remain
// testable without depending on the concrete upstream client.
type Maintenance interface {
	Refresh(context.Context, account.Account) (account.Account, error)
	ProbeFreeQuotaUsage(context.Context, account.Account) (account.UnavailableReason, string, int64, int64, bool, error)
}

type AccountSink interface {
	Upsert(account.Account)
	Delete(string) bool
}

type Service struct {
	runtimeMu        sync.RWMutex
	repository       repository.AccountRepository
	validator        Validator
	maintenance      Maintenance
	sink             AccountSink
	now              func() time.Time
	quotaRetry       time.Duration
	rateRetry        time.Duration
	defaultMaxActive int
}

type Option func(*Service)

func WithSink(sink AccountSink) Option {
	return func(service *Service) {
		service.sink = sink
	}
}

func WithMaintenance(maintenance Maintenance) Option {
	return func(service *Service) {
		service.maintenance = maintenance
	}
}

func WithQuotaRetry(duration time.Duration) Option {
	return func(service *Service) {
		if duration > 0 {
			service.quotaRetry = duration
		}
	}
}

func WithRateRetry(duration time.Duration) Option {
	return func(service *Service) {
		if duration > 0 {
			service.rateRetry = duration
		}
	}
}

// ConfigureRuntime updates maintenance parking backoffs and the default
// per-account concurrency used when imports omit max_active.
func (s *Service) ConfigureRuntime(quotaRetry, rateRetry time.Duration, defaultMaxActive int) {
	if s == nil {
		return
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if quotaRetry > 0 {
		s.quotaRetry = quotaRetry
	}
	if rateRetry > 0 {
		s.rateRetry = rateRetry
	}
	if defaultMaxActive > 0 {
		s.defaultMaxActive = defaultMaxActive
	}
}

func (s *Service) importDefaultMaxActive() int {
	if s == nil {
		return 1
	}
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	if s.defaultMaxActive > 0 {
		return s.defaultMaxActive
	}
	return 1
}

func WithClock(now func() time.Time) Option {
	return func(service *Service) {
		if now != nil {
			service.now = now
		}
	}
}

func NewService(repository repository.AccountRepository, validator Validator, options ...Option) *Service {
	service := &Service{
		repository:       repository,
		validator:        validator,
		now:              time.Now,
		quotaRetry:       24 * time.Hour,
		rateRetry:        45 * time.Second,
		defaultMaxActive: 1,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

var (
	ErrAccountNotFound        = errors.New("account not found")
	ErrInvalidAccountState    = errors.New("invalid account state")
	ErrInvalidBatchAction     = errors.New("invalid batch action")
	ErrMaintenanceUnavailable = errors.New("account maintenance unavailable")
)

// QuotaRefreshResult reports the observation used to update one account.
type QuotaRefreshResult struct {
	AccountID string                    `json:"account_id"`
	Reason    account.UnavailableReason `json:"reason,omitempty"`
	ErrorCode string                    `json:"error_code,omitempty"`
	Actual    int64                     `json:"actual"`
	Limit     int64                     `json:"limit"`
	Observed  bool                      `json:"observed"`
}

// CredentialExport is intentionally returned only by the explicit export
// operation. Normal list/detail DTOs must never embed this type.
type CredentialExport struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	Email        string `json:"email,omitempty"`
	OIDCIssuer   string `json:"oidc_issuer,omitempty"`
	OIDCClientID string `json:"oidc_client_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
}

type UpdateAccountRequest struct {
	Enabled   *bool `json:"enabled,omitempty"`
	Priority  *int  `json:"priority,omitempty"`
	MaxActive *int  `json:"max_active,omitempty"`
}

type BatchAction string

const (
	BatchActionEnable  BatchAction = "enable"
	BatchActionDisable BatchAction = "disable"
	BatchActionRecover BatchAction = "recover"
	BatchActionDelete  BatchAction = "delete"
)

type BatchAccountRequest struct {
	IDs    []string    `json:"ids"`
	Action BatchAction `json:"action"`
}

type BatchAccountResult struct {
	Updated int      `json:"updated"`
	Deleted int      `json:"deleted"`
	IDs     []string `json:"ids"`
}

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

func (s *Service) Get(ctx context.Context, id string) (account.Account, error) {
	item, found, err := s.repository.GetAccount(ctx, strings.TrimSpace(id))
	if err != nil {
		return account.Account{}, fmt.Errorf("get account: %w", err)
	}
	if !found {
		return account.Account{}, ErrAccountNotFound
	}
	return item, nil
}

// RefreshCredential rotates one account's OAuth tokens. A successful token
// exchange does not implicitly recover an unavailable account: validation or
// a quota probe must still establish that it is safe to schedule.
func (s *Service) RefreshCredential(ctx context.Context, id string) (account.Account, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return account.Account{}, err
	}
	if s.maintenance == nil {
		return account.Account{}, ErrMaintenanceUnavailable
	}
	refreshed, refreshErr := s.maintenance.Refresh(ctx, item)
	if refreshErr != nil {
		item.ApplyRefreshFailure(isPermanentRefreshError(refreshErr), s.now().UTC(), 5*time.Minute)
		if err := s.saveAndPublish(ctx, item); err != nil {
			return account.Account{}, err
		}
		return item, fmt.Errorf("refresh credential %s: %w", item.ID, refreshErr)
	}
	refreshed.UpdatedAt = s.now().UTC()
	if err := s.saveAndPublish(ctx, refreshed); err != nil {
		return account.Account{}, err
	}
	return refreshed, nil
}

// RefreshQuota probes the same upstream surface used by production traffic,
// persists observed counters, and updates pool state from the probe result.
func (s *Service) RefreshQuota(ctx context.Context, id string) (QuotaRefreshResult, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return QuotaRefreshResult{}, err
	}
	if s.maintenance == nil {
		return QuotaRefreshResult{}, ErrMaintenanceUnavailable
	}
	reason, errorCode, actual, limit, observed, err := s.maintenance.ProbeFreeQuotaUsage(ctx, item)
	if err != nil {
		return QuotaRefreshResult{}, fmt.Errorf("refresh quota %s: %w", item.ID, err)
	}
	now := s.now().UTC()
	if observed {
		item.SetQuota(actual, limit)
	}
	if reason == "" {
		if !observed && item.QuotaExhausted() {
			item.RecoverQuotaWindow(now)
		} else {
			item.MarkReady(now)
		}
	} else {
		item.MarkUnavailable(reason, s.retryAt(reason, item.RetryAt, now), errorCode, now)
	}
	if err := s.saveAndPublish(ctx, item); err != nil {
		return QuotaRefreshResult{}, err
	}
	return QuotaRefreshResult{
		AccountID: item.ID,
		Reason:    reason,
		ErrorCode: errorCode,
		Actual:    actual,
		Limit:     limit,
		Observed:  observed,
	}, nil
}

func (s *Service) ExportCredential(ctx context.Context, id string) (CredentialExport, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return CredentialExport{}, err
	}
	expiresAt := ""
	if !item.ExpiresAt.IsZero() {
		expiresAt = item.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return CredentialExport{
		ID:           item.ID,
		Key:          item.AccessToken,
		RefreshToken: item.RefreshToken,
		ExpiresAt:    expiresAt,
		Email:        item.Email,
		OIDCIssuer:   item.OIDCIssuer,
		OIDCClientID: item.OIDCClientID,
		UserID:       item.UserID,
		TeamID:       item.TeamID,
	}, nil
}

func (s *Service) saveAndPublish(ctx context.Context, item account.Account) error {
	if err := s.repository.SaveAccount(ctx, item); err != nil {
		return fmt.Errorf("save maintained account %s: %w", item.ID, err)
	}
	if s.sink != nil {
		s.sink.Upsert(item)
	}
	return nil
}

func isPermanentRefreshError(err error) bool {
	var permanent interface{ Permanent() bool }
	return errors.As(err, &permanent) && permanent.Permanent()
}

func (s *Service) Events(ctx context.Context, id string, page, pageSize int) (repository.ListAccountEventsResult, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return repository.ListAccountEventsResult{}, err
	}
	return s.repository.ListAccountEvents(ctx, repository.ListAccountEventsQuery{AccountID: strings.TrimSpace(id), Page: page, PageSize: pageSize})
}

func (s *Service) Update(ctx context.Context, id string, request UpdateAccountRequest) (account.Account, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return account.Account{}, err
	}
	now := s.now().UTC()
	if request.Enabled != nil {
		if *request.Enabled {
			if item.Pool != account.PoolReady && !item.EnableByAdmin(now) {
				return account.Account{}, fmt.Errorf("enable account %s: %w", item.ID, ErrInvalidAccountState)
			}
		} else {
			item.DisableByAdmin(now)
		}
	}
	priority := item.Priority
	if request.Priority != nil {
		priority = *request.Priority
	}
	maxActive := item.MaxActive
	if maxActive < 1 {
		maxActive = 1
	}
	if request.MaxActive != nil {
		maxActive = *request.MaxActive
	}
	if err := item.ConfigureRuntime(priority, maxActive, now); err != nil {
		return account.Account{}, fmt.Errorf("configure account %s: %w", item.ID, err)
	}
	if err := s.repository.SaveAccounts(ctx, []account.Account{item}); err != nil {
		return account.Account{}, fmt.Errorf("save account %s: %w", item.ID, err)
	}
	if s.sink != nil {
		s.sink.Upsert(item)
	}
	return item, nil
}

func (s *Service) Batch(ctx context.Context, request BatchAccountRequest) (BatchAccountResult, error) {
	ids := uniqueAccountIDs(request.IDs)
	if len(ids) == 0 {
		return BatchAccountResult{}, ErrAccountNotFound
	}
	items := make([]account.Account, 0, len(ids))
	for _, id := range ids {
		item, err := s.Get(ctx, id)
		if err != nil {
			return BatchAccountResult{}, err
		}
		items = append(items, item)
	}
	result := BatchAccountResult{IDs: ids}
	if request.Action == BatchActionDelete {
		if err := s.repository.DeleteAccounts(ctx, ids); err != nil {
			return BatchAccountResult{}, fmt.Errorf("delete accounts: %w", err)
		}
		for _, id := range ids {
			if s.sink != nil {
				s.sink.Delete(id)
			}
		}
		result.Deleted = len(ids)
		return result, nil
	}

	now := s.now().UTC()
	for index := range items {
		item := &items[index]
		switch request.Action {
		case BatchActionDisable:
			item.DisableByAdmin(now)
		case BatchActionEnable:
			if item.Pool != account.PoolReady && !item.EnableByAdmin(now) {
				return BatchAccountResult{}, fmt.Errorf("enable account %s: %w", item.ID, ErrInvalidAccountState)
			}
		case BatchActionRecover:
			reason, errorCode, err := s.validator.Validate(ctx, *item)
			if err != nil {
				return BatchAccountResult{}, fmt.Errorf("validate account %s: %w", item.ID, err)
			}
			if reason == "" {
				item.RecoverValidated(now)
			} else {
				item.MarkUnavailable(reason, s.retryAt(reason, item.RetryAt, now), errorCode, now)
			}
		default:
			return BatchAccountResult{}, ErrInvalidBatchAction
		}
	}
	if err := s.repository.SaveAccounts(ctx, items); err != nil {
		return BatchAccountResult{}, fmt.Errorf("save account batch: %w", err)
	}
	for _, item := range items {
		if s.sink != nil {
			s.sink.Upsert(item)
		}
	}
	result.Updated = len(items)
	return result, nil
}

func uniqueAccountIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
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
			// Imports without max_active inherit the global pool max-concurrent
			// setting (hot-updated via ConfigureRuntime).
			item.MaxActive = s.importDefaultMaxActive()
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
			item.MarkReady(now)
		} else {
			item.MarkUnavailable(reason, s.retryAt(reason, item.RetryAt, now), errorCode, now)
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
	if reason == "" {
		item.MarkReady(now)
	} else {
		item.MarkUnavailable(reason, s.retryAt(reason, item.RetryAt, now), errorCode, now)
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
	s.runtimeMu.RLock()
	quotaRetry := s.quotaRetry
	rateRetry := s.rateRetry
	s.runtimeMu.RUnlock()
	switch reason {
	case account.ReasonQuota:
		return now.Add(quotaRetry)
	case account.ReasonCooldown:
		return now.Add(rateRetry)
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
