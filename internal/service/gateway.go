package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/requestctx"
	"github.com/AokiAx/grok2api/internal/scheduler"
	"github.com/AokiAx/grok2api/internal/upstream"
)

type AccountStore interface {
	SaveAccount(context.Context, account.Account) error
}

type Upstream interface {
	Request(context.Context, account.Account, string, string, []byte, bool) (*http.Response, error)
}

type ChatResult struct {
	Status int
	Header http.Header
	Body   []byte
	Stream io.ReadCloser
}

type Gateway struct {
	scheduler  *scheduler.Scheduler
	store      AccountStore
	upstream   Upstream
	quotaRetry time.Duration
	rateRetry  time.Duration
	now        func() time.Time
	circuitMu  sync.Mutex
	circuit    CircuitStatus
}

type CircuitStatus struct {
	Open     bool      `json:"open"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
	Revision uint64    `json:"revision,omitempty"`
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
	upstream Upstream,
	options ...Option,
) *Gateway {
	gateway := &Gateway{
		scheduler:  scheduler,
		store:      store,
		upstream:   upstream,
		quotaRetry: 24 * time.Hour,
		rateRetry:  45 * time.Second,
		now:        time.Now,
	}
	for _, option := range options {
		option(gateway)
	}
	return gateway
}

func (g *Gateway) Chat(ctx context.Context, payload []byte, stream bool) (ChatResult, error) {
	return g.Request(ctx, http.MethodPost, "/chat/completions", payload, stream)
}

func (g *Gateway) Request(
	ctx context.Context,
	method string,
	path string,
	payload []byte,
	stream bool,
) (ChatResult, error) {
	if circuitError := g.quotaCircuitError(); circuitError != nil {
		return ChatResult{}, circuitError
	}
	attempts := g.scheduler.ReadyCount()
	if attempts == 0 {
		return ChatResult{}, g.poolUnavailable()
	}

	attempted := 0
	quotaFailures := 0
	stickyKey := ComposeStickyKey(requestctx.StickyKey(ctx), PayloadAffinityKey(payload))
	for range attempts {
		lease, err := g.scheduler.AcquireSticky(ctx, stickyKey)
		if err != nil {
			if errors.Is(err, scheduler.ErrNoReadyAccount) {
				return ChatResult{}, g.poolUnavailable()
			}
			return ChatResult{}, fmt.Errorf("acquire account: %w", err)
		}
		attempted++
		response, err := g.upstream.Request(
			ctx,
			lease.Account(),
			method,
			path,
			payload,
			stream,
		)
		if err != nil {
			lease.Release()
			g.resetCircuit()
			return ChatResult{}, err
		}
		if stream && response.StatusCode < 400 {
			// Best-effort: never fail a successful upstream response because SQLite is busy.
			_ = g.persistSuccessUsage(ctx, lease, response.Header)
			// Keep returning this successful stream even if remaining hit 0;
			// the account is already marked unavailable for subsequent traffic.
			g.resetCircuit()
			return ChatResult{
				Status: response.StatusCode,
				Header: response.Header.Clone(),
				Stream: &leaseReadCloser{
					body:  response.Body,
					lease: lease,
				},
			}, nil
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
			// Best-effort persist; upstream already succeeded.
			_ = g.persistSuccessUsage(ctx, lease, response.Header)
			// Return the successful body even if this response exhausted free quota.
			lease.Release()
			g.resetCircuit()
			return ChatResult{
				Status: response.StatusCode,
				Header: response.Header.Clone(),
				Body:   body,
			}, nil
		}

		failure := upstream.ClassifyFailure(response.StatusCode, body)
		// Prefer body-reported free quota; fall back to response headers.
		if failure.Kind == upstream.FailureQuota && failure.QuotaLimit == 0 {
			if usage := upstream.ParseRateLimitHeaders(response.Header); usage.Present() {
				failure.QuotaActual = usage.QuotaActual()
				failure.QuotaLimit = usage.QuotaLimit()
			}
		}
		switch failure.Kind {
		case upstream.FailureQuota, upstream.FailureAuth, upstream.FailureRateLimit:
			if failure.Kind == upstream.FailureQuota {
				quotaFailures++
			}
			retryAt := g.now().Add(g.rateRetry)
			if failure.Kind == upstream.FailureQuota {
				retryAt = g.now().Add(g.quotaRetry)
			}
			lease.MoveUnavailable(failure.Reason, retryAt, failure.Code)
			updated := lease.Account()
			if failure.QuotaLimit > 0 || failure.QuotaActual > 0 {
				updated.QuotaActual = failure.QuotaActual
				updated.QuotaLimit = failure.QuotaLimit
			}
			if err := g.store.SaveAccount(ctx, updated); err != nil {
				lease.Release()
				return ChatResult{}, fmt.Errorf("save account transition: %w", err)
			}
			lease.Release()
			continue
		default:
			lease.Release()
			g.resetCircuit()
			return ChatResult{
				Status: response.StatusCode,
				Header: response.Header.Clone(),
				Body:   body,
			}, nil
		}
	}
	if attempted > 0 && quotaFailures == attempted {
		g.openQuotaCircuit()
	}

	return ChatResult{}, g.poolUnavailable()
}

func (g *Gateway) persistSuccessUsage(
	ctx context.Context,
	lease *scheduler.Lease,
	header http.Header,
) error {
	usage := upstream.ParseRateLimitHeaders(header)
	now := g.now().UTC()
	if usage.Present() {
		lease.RecordUsage(usage.QuotaActual(), usage.QuotaLimit(), now)
		if usage.Exhausted() {
			lease.MoveUnavailable(
				account.ReasonQuota,
				now.Add(g.quotaRetry),
				"subscription:free-usage-exhausted",
			)
		}
		if err := g.store.SaveAccount(ctx, lease.Account()); err != nil {
			return fmt.Errorf("save free quota usage: %w", err)
		}
		return nil
	}

	// Even without rate-limit headers, mark last success for ops visibility.
	item := lease.Account()
	if item.ID == "" {
		return nil
	}
	item.LastSuccessAt = now
	item.UpdatedAt = now
	lease.RecordUsage(item.QuotaActual, item.QuotaLimit, now)
	if err := g.store.SaveAccount(ctx, item); err != nil {
		return fmt.Errorf("save account success timestamp: %w", err)
	}
	return nil
}

func (g *Gateway) CircuitStatus() CircuitStatus {
	g.circuitMu.Lock()
	defer g.circuitMu.Unlock()
	g.refreshCircuitLocked()
	return g.circuit
}

func (g *Gateway) quotaCircuitError() *PoolUnavailableError {
	status := g.CircuitStatus()
	if !status.Open {
		return nil
	}
	retryAfter := status.RetryAt.Sub(g.now())
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return &PoolUnavailableError{Status: http.StatusTooManyRequests, RetryAfter: retryAfter}
}

func (g *Gateway) openQuotaCircuit() {
	now := g.now()
	retryAt := now.Add(g.quotaRetry)
	if earliest := g.scheduler.EarliestRetry(); !earliest.IsZero() && earliest.After(now) {
		retryAt = earliest
	}
	g.circuitMu.Lock()
	g.circuit = CircuitStatus{
		Open:     true,
		RetryAt:  retryAt,
		Revision: g.scheduler.Revision(),
	}
	g.circuitMu.Unlock()
}

func (g *Gateway) resetCircuit() {
	g.circuitMu.Lock()
	g.circuit = CircuitStatus{}
	g.circuitMu.Unlock()
}

func (g *Gateway) refreshCircuitLocked() {
	if !g.circuit.Open {
		return
	}
	if g.circuit.Revision != g.scheduler.Revision() || !g.circuit.RetryAt.After(g.now()) {
		g.circuit = CircuitStatus{}
	}
}

type leaseReadCloser struct {
	body  io.ReadCloser
	lease *scheduler.Lease
	once  sync.Once
}

func (r *leaseReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.body.Read(buffer)
	if errors.Is(err, io.EOF) {
		_ = r.Close()
	}
	return count, err
}

func (r *leaseReadCloser) Close() error {
	var closeErr error
	r.once.Do(func() {
		closeErr = r.body.Close()
		r.lease.Release()
	})
	return closeErr
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
		retryAfter = earliest.Sub(g.now())
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
	}
	return &PoolUnavailableError{
		Status:     http.StatusTooManyRequests,
		RetryAfter: retryAfter,
	}
}
