package runtime

import (
	"context"
	"fmt"
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
// Prefer a real chat/rate-limit probe over a pure timer.
type QuotaProber interface {
	ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error)
}

type CredentialRecoveryResult struct {
	Recovered int
	Failed    int
	Skipped   int
}

type QuotaRecoveryResult struct {
	Recovered int
	Deferred  int
	Failed    int
	Skipped   int
}

func RecoverCredentials(
	ctx context.Context,
	scheduler *scheduler.Scheduler,
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
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable ||
			item.UnavailableReason != account.ReasonAuth ||
			item.RefreshToken == "" ||
			(!item.RetryAt.IsZero() && item.RetryAt.After(now)) {
			result.Skipped++
			continue
		}
		refreshed, refreshErr := refresher.Refresh(ctx, item)
		if refreshErr != nil {
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = account.ReasonAuth
			item.RetryAt = now.Add(30 * time.Minute)
			item.LastErrorCode = "refresh-failed"
			item.UpdatedAt = now.UTC()
			if err := store.SaveAccount(ctx, item); err != nil {
				return result, fmt.Errorf("save refresh failure for %s: %w", item.ID, err)
			}
			scheduler.Upsert(item)
			result.Failed++
			continue
		}
		reason, errorCode, validateErr := validator.Validate(ctx, refreshed)
		if validateErr != nil {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = account.ReasonAuth
			refreshed.RetryAt = now.Add(30 * time.Minute)
			refreshed.LastErrorCode = "validation-failed"
			result.Failed++
		} else if reason == "" {
			refreshed.Pool = account.PoolReady
			refreshed.UnavailableReason = ""
			refreshed.RetryAt = time.Time{}
			refreshed.LastErrorCode = ""
			refreshed.AuthenticationFails = 0
			result.Recovered++
		} else {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = reason
			refreshed.LastErrorCode = errorCode
			refreshed.RetryAt = credentialRetryAt(reason, now)
			result.Failed++
		}
		refreshed.UpdatedAt = now.UTC()
		if err := store.SaveAccount(ctx, refreshed); err != nil {
			return result, fmt.Errorf("save refreshed account %s: %w", refreshed.ID, err)
		}
		scheduler.Upsert(refreshed)
	}
	return result, nil
}

func credentialRetryAt(reason account.UnavailableReason, now time.Time) time.Time {
	switch reason {
	case account.ReasonQuota:
		return now.Add(30 * time.Minute)
	case account.ReasonCooldown:
		return now.Add(45 * time.Second)
	case account.ReasonAuth, account.ReasonValidating:
		return now.Add(30 * time.Minute)
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
		quotaRetry = 30 * time.Minute
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return QuotaRecoveryResult{}, fmt.Errorf("list accounts for quota recovery: %w", err)
	}
	result := QuotaRecoveryResult{}
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonQuota {
			result.Skipped++
			continue
		}
		if !item.RetryAt.IsZero() && item.RetryAt.After(now) {
			result.Skipped++
			continue
		}

		var (
			reason    account.UnavailableReason
			errorCode string
			probeErr  error
		)
		if prober != nil {
			reason, errorCode, probeErr = prober.ProbeFreeQuota(ctx, item)
		} else {
			reason, errorCode, probeErr = validator.Validate(ctx, item)
		}
		if probeErr != nil {
			// Transport/infra failures: keep unavailable and retry later without
			// treating the account as still quota-exhausted.
			item.RetryAt = now.Add(5 * time.Minute)
			item.LastErrorCode = "quota-probe-error"
			item.UpdatedAt = now.UTC()
			if err := store.SaveAccount(ctx, item); err != nil {
				return result, fmt.Errorf("save quota probe error for %s: %w", item.ID, err)
			}
			pool.Upsert(item)
			result.Failed++
			continue
		}
		if reason == "" {
			item.Pool = account.PoolReady
			item.UnavailableReason = ""
			item.RetryAt = time.Time{}
			item.LastErrorCode = ""
			item.UpdatedAt = now.UTC()
			if err := store.SaveAccount(ctx, item); err != nil {
				return result, fmt.Errorf("save recovered quota account %s: %w", item.ID, err)
			}
			pool.Upsert(item)
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
		item.UpdatedAt = now.UTC()
		if err := store.SaveAccount(ctx, item); err != nil {
			return result, fmt.Errorf("save deferred quota account %s: %w", item.ID, err)
		}
		pool.Upsert(item)
		if reason == account.ReasonQuota {
			result.Deferred++
		} else {
			result.Failed++
		}
	}
	return result, nil
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
			return fmt.Errorf("save promoted account %s: %w", item.ID, err)
		}
	}
	return nil
}

func RunRecovery(
	ctx context.Context,
	scheduler *scheduler.Scheduler,
	store AccountStore,
	interval time.Duration,
	options ...RecoveryOption,
) error {
	config := recoveryConfig{quotaRetry: 30 * time.Minute}
	for _, option := range options {
		option(&config)
	}
	runOnce := func(now time.Time) error {
		if err := RecoverDue(ctx, scheduler, store, now); err != nil {
			return err
		}
		if config.credentialStore != nil && (config.quotaProber != nil || config.validator != nil) {
			if _, err := RecoverQuota(
				ctx,
				scheduler,
				config.credentialStore,
				config.quotaProber,
				config.validator,
				now,
				config.quotaRetry,
			); err != nil {
				return err
			}
		}
		if config.credentialStore != nil && config.refresher != nil && config.validator != nil {
			if _, err := RecoverCredentials(
				ctx,
				scheduler,
				config.credentialStore,
				config.refresher,
				config.validator,
				now,
			); err != nil {
				return err
			}
		}
		return nil
	}
	if err := runOnce(time.Now().UTC()); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := runOnce(now); err != nil {
				return err
			}
		}
	}
}
