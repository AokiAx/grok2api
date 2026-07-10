package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/scheduler"
	"github.com/AokiAx/grok2api/internal/upstream"
)

type AccountStore interface {
	SaveAccount(context.Context, account.Account) error
}

type ChatUpstream interface {
	Chat(context.Context, account.Account, []byte, bool) (*http.Response, error)
}

type ChatResult struct {
	Status int
	Header http.Header
	Body   []byte
}

type Gateway struct {
	scheduler  *scheduler.Scheduler
	store      AccountStore
	upstream   ChatUpstream
	quotaRetry time.Duration
	rateRetry  time.Duration
	now        func() time.Time
}

type Option func(*Gateway)

func WithQuotaRetry(duration time.Duration) Option {
	return func(gateway *Gateway) {
		gateway.quotaRetry = duration
	}
}

func WithRateRetry(duration time.Duration) Option {
	return func(gateway *Gateway) {
		gateway.rateRetry = duration
	}
}

func NewGateway(
	scheduler *scheduler.Scheduler,
	store AccountStore,
	upstream ChatUpstream,
	options ...Option,
) *Gateway {
	gateway := &Gateway{
		scheduler:  scheduler,
		store:      store,
		upstream:   upstream,
		quotaRetry: 30 * time.Minute,
		rateRetry:  45 * time.Second,
		now:        time.Now,
	}
	for _, option := range options {
		option(gateway)
	}
	return gateway
}

func (g *Gateway) Chat(ctx context.Context, payload []byte, stream bool) (ChatResult, error) {
	attempts := g.scheduler.ReadyCount()
	if attempts == 0 {
		return ChatResult{}, g.poolUnavailable()
	}

	for range attempts {
		lease, err := g.scheduler.Acquire(ctx)
		if err != nil {
			if errors.Is(err, scheduler.ErrNoReadyAccount) {
				return ChatResult{}, g.poolUnavailable()
			}
			return ChatResult{}, fmt.Errorf("acquire account: %w", err)
		}
		response, err := g.upstream.Chat(ctx, lease.Account(), payload, stream)
		if err != nil {
			lease.Release()
			return ChatResult{}, err
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			lease.Release()
			return ChatResult{}, fmt.Errorf("read upstream response: %w", readErr)
		}
		if closeErr != nil {
			lease.Release()
			return ChatResult{}, fmt.Errorf("close upstream response: %w", closeErr)
		}
		if response.StatusCode < 400 {
			lease.Release()
			return ChatResult{
				Status: response.StatusCode,
				Header: response.Header.Clone(),
				Body:   body,
			}, nil
		}

		failure := upstream.ClassifyFailure(response.StatusCode, body)
		switch failure.Kind {
		case upstream.FailureQuota, upstream.FailureAuth, upstream.FailureRateLimit:
			retryAt := g.now().Add(g.rateRetry)
			if failure.Kind == upstream.FailureQuota {
				retryAt = g.now().Add(g.quotaRetry)
			}
			lease.MoveUnavailable(failure.Reason, retryAt, failure.Code)
			updated := lease.Account()
			updated.QuotaActual = failure.QuotaActual
			updated.QuotaLimit = failure.QuotaLimit
			if err := g.store.SaveAccount(ctx, updated); err != nil {
				lease.Release()
				return ChatResult{}, fmt.Errorf("save account transition: %w", err)
			}
			lease.Release()
			continue
		default:
			lease.Release()
			return ChatResult{
				Status: response.StatusCode,
				Header: response.Header.Clone(),
				Body:   body,
			}, nil
		}
	}

	return ChatResult{}, g.poolUnavailable()
}

type PoolUnavailableError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *PoolUnavailableError) Error() string {
	return "ready account pool is empty"
}

func AsPoolUnavailable(err error) (*PoolUnavailableError, bool) {
	var target *PoolUnavailableError
	ok := errors.As(err, &target)
	return target, ok
}

func (g *Gateway) poolUnavailable() *PoolUnavailableError {
	retryAfter := g.quotaRetry
	if earliest := g.scheduler.EarliestRetry(); !earliest.IsZero() {
		retryAfter = time.Until(earliest)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
	}
	return &PoolUnavailableError{
		Status:     http.StatusTooManyRequests,
		RetryAfter: retryAfter,
	}
}
