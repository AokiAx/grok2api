package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/scheduler"
)

type AccountStore interface {
	SaveAccount(context.Context, account.Account) error
}

type CredentialStore interface {
	AccountStore
	ListAccounts(context.Context) ([]account.Account, error)
}

type CredentialRefresher interface {
	Refresh(context.Context, account.Account) (account.Account, error)
}

type CredentialValidator interface {
	Validate(context.Context, account.Account) (account.UnavailableReason, string, error)
}

// QuotaProber verifies free-tier capacity before re-entry.
// Prefer a real /responses probe (same path as live chat) over a pure timer.
type QuotaProber interface {
	ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error)
}

// QuotaUsageProber optionally returns free-tier used/limit observed on the probe.
type QuotaUsageProber interface {
	ProbeFreeQuotaUsage(context.Context, account.Account) (account.UnavailableReason, string, int64, int64, bool, error)
}

type CredentialRecoveryResult struct {
	Recovered int
	Failed    int
	Skipped   int
	Refreshed int // proactive ready refreshes
	Revoked   int // permanent invalid_grant / revoked refresh
}

type QuotaRecoveryResult struct {
	Recovered int
	Deferred  int
	Failed    int
	Skipped   int
}

const (
	// Per recovery tick (default interval ~20s). Sized for multi-thousand
	// account pools so expired access tokens drain in minutes, not hours.
	maxQuotaProbesPerTick        = 32
	maxAuthRefreshesPerTick      = 256
	maxProactiveRefreshesPerTick = 256
	maxIsolatePerTick            = 500
	proactiveRefreshLead         = 90 * time.Minute
	authTransientBackoff         = 5 * time.Minute
	authValidationBackoff        = 10 * time.Minute
)

// IsolationResult counts permanent auth quarantines.
type IsolationResult struct {
	Isolated int
	Skipped  int
	Failed   int
}

// IsolateUnrecoverableAuth moves permanently dead credentials out of the auth
// recovery queue into reason=disabled so status/ops can see them separately
// and recovery ticks stop wasting refresh budget on them.
func IsolateUnrecoverableAuth(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	now time.Time,
) (IsolationResult, error) {
	if store == nil {
		return IsolationResult{}, nil
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return IsolationResult{}, fmt.Errorf("list accounts for auth isolation: %w", err)
	}
	result := IsolationResult{}
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable {
			result.Skipped++
			continue
		}
		// Already quarantined.
		if item.UnavailableReason == account.ReasonDisabled && isUnrecoverableAuthCode(item.LastErrorCode) {
			result.Skipped++
			continue
		}
		if !isUnrecoverableAuthCode(item.LastErrorCode) {
			result.Skipped++
			continue
		}
		if result.Isolated >= maxIsolatePerTick {
			result.Skipped++
			continue
		}
		isolateRevokedAccount(&item, now)
		if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
			result.Failed++
			continue
		}
		result.Isolated++
	}
	return result, nil
}

func isolateRevokedAccount(item *account.Account, now time.Time) {
	item.Pool = account.PoolUnavailable
	item.UnavailableReason = account.ReasonDisabled
	if !isUnrecoverableAuthCode(item.LastErrorCode) {
		item.LastErrorCode = "refresh-revoked"
	}
	// Far-future: never auto-retry; operator must re-import.
	item.RetryAt = now.Add(365 * 24 * time.Hour)
	item.UpdatedAt = now.UTC()
}

func RecoverCredentials(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	refresher CredentialRefresher,
	validator CredentialValidator,
	now time.Time,
) (CredentialRecoveryResult, error) {
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return CredentialRecoveryResult{}, fmt.Errorf("list accounts for credential recovery: %w", err)
	}
	result := CredentialRecoveryResult{}
	candidates := make([]account.Account, 0, 256)
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable ||
			item.UnavailableReason != account.ReasonAuth ||
			item.RefreshToken == "" {
			result.Skipped++
			continue
		}
		// Permanent failures: quarantine and stop burning refresh budget.
		if isUnrecoverableAuthCode(item.LastErrorCode) {
			isolateRevokedAccount(&item, now)
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
			} else {
				result.Revoked++
			}
			continue
		}
		if !item.RetryAt.IsZero() && item.RetryAt.After(now) {
			result.Skipped++
			continue
		}
		candidates = append(candidates, item)
	}
	// Prefer oldest updated / earliest expiry so stuck auth_failed drain first.
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i], candidates[j]
		if !ai.ExpiresAt.Equal(aj.ExpiresAt) {
			if ai.ExpiresAt.IsZero() {
				return false
			}
			if aj.ExpiresAt.IsZero() {
				return true
			}
			return ai.ExpiresAt.Before(aj.ExpiresAt)
		}
		return ai.UpdatedAt.Before(aj.UpdatedAt)
	})
	if len(candidates) > maxAuthRefreshesPerTick {
		result.Skipped += len(candidates) - maxAuthRefreshesPerTick
		candidates = candidates[:maxAuthRefreshesPerTick]
	}
	for _, item := range candidates {
		refreshed, refreshErr := refresher.Refresh(ctx, item)
		if refreshErr != nil {
			applyRefreshFailure(&item, refreshErr, now)
			if isUnrecoverableAuthCode(item.LastErrorCode) {
				result.Revoked++
			} else {
				result.Failed++
			}
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
			}
			continue
		}
		reason, errorCode, validateErr := validator.Validate(ctx, refreshed)
		if validateErr != nil {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = account.ReasonAuth
			refreshed.RetryAt = now.Add(authValidationBackoff)
			refreshed.LastErrorCode = "validation-failed"
			result.Failed++
		} else if reason == "" {
			refreshed.Pool = account.PoolReady
			refreshed.UnavailableReason = ""
			refreshed.RetryAt = time.Time{}
			refreshed.LastErrorCode = ""
			refreshed.AuthenticationFails = 0
			result.Recovered++
		} else if isUnrecoverableAuthCode(errorCode) {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = account.ReasonDisabled
			refreshed.LastErrorCode = errorCode
			refreshed.RetryAt = now.Add(365 * 24 * time.Hour)
			result.Revoked++
		} else {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = reason
			refreshed.LastErrorCode = errorCode
			refreshed.RetryAt = credentialRetryAt(reason, now)
			result.Failed++
		}
		refreshed.UpdatedAt = now.UTC()
		if err := saveAccountBestEffort(ctx, store, pool, refreshed); err != nil {
			result.Failed++
			continue
		}
	}
	return result, nil
}

// RefreshExpiring proactively refreshes ready accounts whose access tokens are
// near expiry (or already past expires_at), so live traffic does not wait for 401.
// Candidates are processed soonest-expiry first so backlog drains fairly.
func RefreshExpiring(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	refresher CredentialRefresher,
	now time.Time,
	lead time.Duration,
) (CredentialRecoveryResult, error) {
	if store == nil || refresher == nil {
		return CredentialRecoveryResult{}, nil
	}
	if lead <= 0 {
		lead = proactiveRefreshLead
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return CredentialRecoveryResult{}, fmt.Errorf("list accounts for proactive refresh: %w", err)
	}
	result := CredentialRecoveryResult{}
	deadline := now.Add(lead)
	candidates := make([]account.Account, 0, len(accounts))
	for _, item := range accounts {
		if item.Pool != account.PoolReady {
			result.Skipped++
			continue
		}
		if item.RefreshToken == "" || item.OIDCIssuer == "" || item.OIDCClientID == "" {
			result.Skipped++
			continue
		}
		// Unknown expiry: leave alone. Known and still far out: skip.
		if item.ExpiresAt.IsZero() || item.ExpiresAt.After(deadline) {
			result.Skipped++
			continue
		}
		candidates = append(candidates, item)
	}
	// Already-expired and soonest-to-expire first.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ExpiresAt.Before(candidates[j].ExpiresAt)
	})
	if len(candidates) > maxProactiveRefreshesPerTick {
		result.Skipped += len(candidates) - maxProactiveRefreshesPerTick
		candidates = candidates[:maxProactiveRefreshesPerTick]
	}
	for _, item := range candidates {
		refreshed, refreshErr := refresher.Refresh(ctx, item)
		if refreshErr != nil {
			applyRefreshFailure(&item, refreshErr, now)
			if isUnrecoverableAuthCode(item.LastErrorCode) {
				result.Revoked++
			} else {
				result.Failed++
			}
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
			}
			continue
		}
		// Keep ready; only rotate credentials.
		refreshed.Pool = account.PoolReady
		refreshed.UnavailableReason = ""
		refreshed.RetryAt = time.Time{}
		refreshed.LastErrorCode = ""
		refreshed.UpdatedAt = now.UTC()
		if err := saveAccountBestEffort(ctx, store, pool, refreshed); err != nil {
			result.Failed++
			continue
		}
		result.Refreshed++
	}
	return result, nil
}

func applyRefreshFailure(item *account.Account, refreshErr error, now time.Time) {
	item.Pool = account.PoolUnavailable
	item.UpdatedAt = now.UTC()
	item.AuthenticationFails++
	if isPermanentRefresh(refreshErr) {
		// Quarantine permanently — do not keep them in the auth retry queue.
		item.UnavailableReason = account.ReasonDisabled
		item.LastErrorCode = "refresh-revoked"
		item.RetryAt = now.Add(365 * 24 * time.Hour)
		return
	}
	item.UnavailableReason = account.ReasonAuth
	item.LastErrorCode = "refresh-failed"
	item.RetryAt = now.Add(authTransientBackoff)
}

func isPermanentRefresh(err error) bool {
	if err == nil {
		return false
	}
	var marker interface{ Permanent() bool }
	if errors.As(err, &marker) && marker.Permanent() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "revoked") ||
		strings.Contains(msg, "refresh token has been")
}

func isUnrecoverableAuthCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "refresh-revoked", "invalid_grant":
		return true
	default:
		return false
	}
}

func credentialRetryAt(reason account.UnavailableReason, now time.Time) time.Time {
	switch reason {
	case account.ReasonQuota:
		// Free usage is a rolling ~24h window; short retries just burn probes.
		return now.Add(24 * time.Hour)
	case account.ReasonCooldown:
		return now.Add(45 * time.Second)
	case account.ReasonAuth, account.ReasonValidating:
		return now.Add(authTransientBackoff)
	case account.ReasonDisabled:
		return now.Add(365 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}

type recoveryConfig struct {
	credentialStore CredentialStore
	refresher       CredentialRefresher
	validator       CredentialValidator
	quotaProber     QuotaProber
	quotaRetry      time.Duration
}

type RecoveryOption func(*recoveryConfig)

func WithCredentialRecovery(
	store CredentialStore,
	refresher CredentialRefresher,
	validator CredentialValidator,
) RecoveryOption {
	return func(config *recoveryConfig) {
		config.credentialStore = store
		config.refresher = refresher
		config.validator = validator
	}
}

// WithQuotaRetry sets the backoff used when a quota probe still reports exhausted.
func WithQuotaRetry(duration time.Duration) RecoveryOption {
	return func(config *recoveryConfig) {
		if duration > 0 {
			config.quotaRetry = duration
		}
	}
}

// WithQuotaProber installs the free-quota probe used by RecoverQuota.
func WithQuotaProber(prober QuotaProber) RecoveryOption {
	return func(config *recoveryConfig) {
		config.quotaProber = prober
	}
}

// RecoverQuota probes due free-quota accounts before re-entry.
// Unlike cooldown, quota is NOT blindly promoted by retry_at alone:
// we only return an account to ready when a real probe succeeds.
func RecoverQuota(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	prober QuotaProber,
	validator CredentialValidator,
	now time.Time,
	quotaRetry time.Duration,
) (QuotaRecoveryResult, error) {
	if store == nil || (prober == nil && validator == nil) {
		return QuotaRecoveryResult{}, nil
	}
	if quotaRetry <= 0 {
		quotaRetry = 24 * time.Hour
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return QuotaRecoveryResult{}, fmt.Errorf("list accounts for quota recovery: %w", err)
	}
	result := QuotaRecoveryResult{}
	probed := 0
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonQuota {
			result.Skipped++
			continue
		}
		if !item.RetryAt.IsZero() && item.RetryAt.After(now) {
			result.Skipped++
			continue
		}
		if probed >= maxQuotaProbesPerTick {
			// Leave remaining due accounts for the next worker tick.
			result.Skipped++
			continue
		}
		probed++

		reason, errorCode, actual, limit, hasUsage, probeErr := probeQuota(ctx, prober, validator, item)
		if probeErr != nil {
			// Transport/infra failures: keep unavailable and retry later without
			// treating the account as still quota-exhausted.
			item.RetryAt = now.Add(5 * time.Minute)
			item.LastErrorCode = "quota-probe-error"
			item.UpdatedAt = now.UTC()
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
				continue
			}
			result.Failed++
			continue
		}
		if reason == "" {
			item.Pool = account.PoolReady
			item.UnavailableReason = ""
			item.RetryAt = time.Time{}
			item.LastErrorCode = ""
			item.UpdatedAt = now.UTC()
			if hasUsage {
				item.QuotaActual = actual
				item.QuotaLimit = limit
				item.LastSuccessAt = now.UTC()
			} else if item.QuotaLimit > 0 && item.QuotaActual >= item.QuotaLimit {
				// Clear "looks full" counters when probe succeeded without headers.
				item.QuotaActual = 0
			}
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
				continue
			}
			result.Recovered++
			continue
		}

		// Still bad: keep unavailable with reason-specific backoff.
		item.Pool = account.PoolUnavailable
		item.UnavailableReason = reason
		item.LastErrorCode = firstNonEmpty(errorCode, "quota-probe-failed")
		item.RetryAt = credentialRetryAt(reason, now)
		if reason == account.ReasonQuota {
			item.RetryAt = now.Add(quotaRetry)
		}
		if hasUsage {
			item.QuotaActual = actual
			item.QuotaLimit = limit
		}
		item.UpdatedAt = now.UTC()
		if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
			result.Failed++
			continue
		}
		if reason == account.ReasonQuota {
			result.Deferred++
		} else {
			result.Failed++
		}
	}
	return result, nil
}

func probeQuota(
	ctx context.Context,
	prober QuotaProber,
	validator CredentialValidator,
	item account.Account,
) (account.UnavailableReason, string, int64, int64, bool, error) {
	if usageProber, ok := prober.(QuotaUsageProber); ok {
		return usageProber.ProbeFreeQuotaUsage(ctx, item)
	}
	if prober != nil {
		reason, code, err := prober.ProbeFreeQuota(ctx, item)
		return reason, code, 0, 0, false, err
	}
	reason, code, err := validator.Validate(ctx, item)
	return reason, code, 0, 0, false, err
}

func saveAccountBestEffort(
	ctx context.Context,
	store AccountStore,
	pool *scheduler.Scheduler,
	item account.Account,
) error {
	if err := store.SaveAccount(ctx, item); err != nil {
		slog.Error("recovery save account failed", "account_id", item.ID, "error", err)
		return err
	}
	if pool != nil {
		pool.Upsert(item)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func RecoverDue(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store AccountStore,
	now time.Time,
) error {
	// Only cooldown re-enters without an upstream probe.
	// Quota is recovered via RecoverQuota after a real probe.
	for _, item := range pool.PromoteDue(now) {
		if err := store.SaveAccount(ctx, item); err != nil {
			// Do not abort the whole recovery tick for one account.
			slog.Error("recovery promote save failed", "account_id", item.ID, "error", err)
			continue
		}
	}
	return nil
}

func RunRecovery(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store AccountStore,
	interval time.Duration,
	options ...RecoveryOption,
) error {
	config := recoveryConfig{quotaRetry: 24 * time.Hour}
	for _, option := range options {
		option(&config)
	}
	runOnce := func(now time.Time) {
		if err := RecoverDue(ctx, pool, store, now); err != nil {
			slog.Error("recovery promote tick failed", "error", err)
		}
		// Quarantine permanently dead refresh tokens before spending refresh budget.
		if config.credentialStore != nil {
			if iso, err := IsolateUnrecoverableAuth(ctx, pool, config.credentialStore, now); err != nil {
				slog.Error("recovery auth isolation tick failed", "error", err)
			} else if iso.Isolated > 0 {
				slog.Info("recovery isolated revoked accounts", "isolated", iso.Isolated, "failed", iso.Failed)
			}
		}
		if config.credentialStore != nil && (config.quotaProber != nil || config.validator != nil) {
			if _, err := RecoverQuota(
				ctx,
				pool,
				config.credentialStore,
				config.quotaProber,
				config.validator,
				now,
				config.quotaRetry,
			); err != nil {
				slog.Error("recovery quota tick failed", "error", err)
			}
		}
		if config.credentialStore != nil && config.refresher != nil {
			if _, err := RefreshExpiring(
				ctx,
				pool,
				config.credentialStore,
				config.refresher,
				now,
				proactiveRefreshLead,
			); err != nil {
				slog.Error("recovery proactive refresh tick failed", "error", err)
			}
		}
		if config.credentialStore != nil && config.refresher != nil && config.validator != nil {
			if _, err := RecoverCredentials(
				ctx,
				pool,
				config.credentialStore,
				config.refresher,
				config.validator,
				now,
			); err != nil {
				slog.Error("recovery credential tick failed", "error", err)
			}
		}
	}
	runOnce(time.Now().UTC())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			runOnce(now.UTC())
		}
	}
}
