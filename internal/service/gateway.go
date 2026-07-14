package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	scheduler       *scheduler.Scheduler
	store           AccountStore
	upstream        Upstream
	quotaRetry      time.Duration
	rateRetry       time.Duration
	validatingRetry time.Duration
	// maxAttempts caps how many accounts one request may park while rotating.
	// Default 3 — never equal to ReadyCount() (that burns the whole pool).
	maxAttempts int
	// acquireTimeout bounds how long AcquireSticky may wait for capacity.
	// Zero means no extra timeout (use request context only).
	acquireTimeout time.Duration
	now            func() time.Time
	circuitMu      sync.Mutex
	circuit        CircuitStatus
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

func WithValidatingRetry(duration time.Duration) Option {
	return func(gateway *Gateway) {
		gateway.validatingRetry = duration
	}
}

// WithMaxAttempts sets the per-request account rotation budget.
// Values <= 0 fall back to 3.
func WithMaxAttempts(n int) Option {
	return func(gateway *Gateway) {
		gateway.maxAttempts = n
	}
}

// WithAcquireTimeout bounds waiting for a free account lease.
func WithAcquireTimeout(duration time.Duration) Option {
	return func(gateway *Gateway) {
		gateway.acquireTimeout = duration
	}
}

func NewGateway(
	scheduler *scheduler.Scheduler,
	store AccountStore,
	upstream Upstream,
	options ...Option,
) *Gateway {
	gateway := &Gateway{
		scheduler:       scheduler,
		store:           store,
		upstream:        upstream,
		quotaRetry:      24 * time.Hour,
		rateRetry:       45 * time.Second,
		validatingRetry: 45 * time.Second,
		maxAttempts:     3,
		now:             time.Now,
	}
	for _, option := range options {
		option(gateway)
	}
	if gateway.maxAttempts <= 0 {
		gateway.maxAttempts = 3
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
	ready := g.scheduler.ReadyCount()
	if ready == 0 {
		return ChatResult{}, g.poolUnavailable()
	}
	// Critical: never rotate through the entire ready pool. One exhausted or
	// permission-denied response would otherwise park every credential.
	attempts := g.maxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	if ready < attempts {
		attempts = ready
	}

	attempted := 0
	quotaFailures := 0
	promptCacheKey := PromptCacheKeyFromPayload(payload)
	stickyKey := ComposeStickyKeyParts(
		requestctx.StickyKey(ctx),
		promptCacheKey,
		PayloadAffinityKey(payload),
	)
	// Prefer official prompt_cache_key for upstream x-grok-conv-id continuity.
	if promptCacheKey != "" && strings.TrimSpace(upstream.ConvIDFrom(ctx)) == "" {
		ctx = upstream.WithConvID(ctx, promptCacheKey)
	}
	for range attempts {
		acquireCtx := ctx
		var cancel context.CancelFunc
		if g.acquireTimeout > 0 {
			acquireCtx, cancel = context.WithTimeout(ctx, g.acquireTimeout)
		}
		lease, err := g.scheduler.AcquireSticky(acquireCtx, stickyKey)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			if errors.Is(err, scheduler.ErrNoReadyAccount) {
				return ChatResult{}, g.poolUnavailableFrom(err)
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return ChatResult{}, g.poolUnavailableFrom(&scheduler.SelectionError{
					Reason:     scheduler.SelectionSaturated,
					RetryAfter: time.Second,
				})
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
		// Account-level failures: park this credential and try the next ready
		// account. Includes transient 403 permission-denied (ReasonValidating)
		// so provisioning lag does not surface as a client-facing 403.
		if shouldRotateAccount(failure) {
			if failure.Kind == upstream.FailureQuota {
				quotaFailures++
			}
			retryAt := g.retryAtFor(failure)
			reason := failure.Reason
			if reason == "" {
				reason = account.ReasonCooldown
			}
			lease.MoveUnavailable(reason, retryAt, failure.Code)
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
		}
		lease.Release()
		g.resetCircuit()
		return ChatResult{
			Status: response.StatusCode,
			Header: response.Header.Clone(),
			Body:   body,
		}, nil
	}
	if attempted > 0 && quotaFailures == attempted {
		g.openQuotaCircuit()
		if err := g.quotaCircuitError(); err != nil {
			return ChatResult{}, err
		}
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
	return &PoolUnavailableError{
		Status:     http.StatusTooManyRequests,
		RetryAfter: retryAfter,
		Reason:     PoolReasonCircuit,
		Message:    "quota circuit open; retry later",
	}
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

// shouldRotateAccount reports whether this upstream failure is tied to the
// current credential/pool state (not the client request). Those accounts are
// parked and the request is retried on another ready credential.
func shouldRotateAccount(failure upstream.Failure) bool {
	switch failure.Kind {
	case upstream.FailureQuota, upstream.FailureAuth, upstream.FailureRateLimit:
		return true
	}
	// Transient post-mint chat denials (403 permission-denied) land as
	// FailureUpstream + ReasonValidating so import does not quarantine them as
	// auth. Live traffic must still rotate instead of returning 403 to clients.
	if failure.Reason == account.ReasonValidating {
		return true
	}
	return false
}

func (g *Gateway) retryAtFor(failure upstream.Failure) time.Time {
	now := g.now()
	switch {
	case failure.Kind == upstream.FailureQuota:
		return now.Add(g.quotaRetry)
	case failure.Reason == account.ReasonValidating:
		retry := g.validatingRetry
		if retry <= 0 {
			retry = 45 * time.Second
		}
		return now.Add(retry)
	default:
		return now.Add(g.rateRetry)
	}
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

// PoolUnavailableReason is a client-facing selection failure code.
type PoolUnavailableReason string

const (
	PoolReasonEmpty      PoolUnavailableReason = "no_ready"
	PoolReasonSaturated  PoolUnavailableReason = "saturated"
	PoolReasonQuota      PoolUnavailableReason = "quota"
	PoolReasonCooling    PoolUnavailableReason = "cooling"
	PoolReasonAuth       PoolUnavailableReason = "auth"
	PoolReasonValidating PoolUnavailableReason = "validating"
	PoolReasonCircuit    PoolUnavailableReason = "quota_circuit"
)

// PoolUnavailableError is returned when the gateway cannot lease a usable account.
type PoolUnavailableError struct {
	Status     int
	RetryAfter time.Duration
	Reason     PoolUnavailableReason
	Message    string
}

func (e *PoolUnavailableError) Error() string {
	if e == nil {
		return "ready account pool is empty"
	}
	if e.Message != "" {
		return e.Message
	}
	switch e.Reason {
	case PoolReasonSaturated:
		return "ready accounts are at concurrency capacity"
	case PoolReasonQuota:
		return "no ready accounts (quota exhausted)"
	case PoolReasonCooling:
		return "no ready accounts (cooling down)"
	case PoolReasonAuth:
		return "no ready accounts (auth failures)"
	case PoolReasonValidating:
		return "no ready accounts (validating)"
	case PoolReasonCircuit:
		return "quota circuit open; retry later"
	default:
		return "ready account pool is empty"
	}
}

func AsPoolUnavailable(err error) (*PoolUnavailableError, bool) {
	var target *PoolUnavailableError
	ok := errors.As(err, &target)
	return target, ok
}

func (g *Gateway) poolUnavailable() *PoolUnavailableError {
	return g.poolUnavailableFrom(nil)
}

func (g *Gateway) poolUnavailableFrom(err error) *PoolUnavailableError {
	if sel, ok := scheduler.AsSelectionError(err); ok && sel != nil {
		return mapSelectionError(sel)
	}
	retryAfter := g.quotaRetry
	if earliest := g.scheduler.EarliestRetry(); !earliest.IsZero() {
		retryAfter = earliest.Sub(g.now())
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
	}
	// Infer reason from pool status when acquire did not return a SelectionError.
	_, _, reasons := g.scheduler.Status()
	reason := PoolReasonEmpty
	switch {
	case reasons[account.ReasonQuota] > 0:
		reason = PoolReasonQuota
	case reasons[account.ReasonCooldown] > 0:
		reason = PoolReasonCooling
	case reasons[account.ReasonAuth] > 0:
		reason = PoolReasonAuth
	case reasons[account.ReasonValidating] > 0:
		reason = PoolReasonValidating
	}
	status := http.StatusServiceUnavailable
	if reason == PoolReasonQuota {
		status = http.StatusTooManyRequests
	}
	return &PoolUnavailableError{
		Status:     status,
		RetryAfter: retryAfter,
		Reason:     reason,
	}
}

func mapSelectionError(sel *scheduler.SelectionError) *PoolUnavailableError {
	retryAfter := sel.RetryAfter
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	out := &PoolUnavailableError{
		RetryAfter: retryAfter,
		Message:    sel.Error(),
	}
	switch sel.Reason {
	case scheduler.SelectionSaturated:
		out.Reason = PoolReasonSaturated
		out.Status = http.StatusServiceUnavailable
		if retryAfter < 5*time.Second {
			out.RetryAfter = time.Second
		}
	case scheduler.SelectionQuota:
		out.Reason = PoolReasonQuota
		out.Status = http.StatusTooManyRequests
	case scheduler.SelectionCooling:
		out.Reason = PoolReasonCooling
		out.Status = http.StatusServiceUnavailable
	case scheduler.SelectionAuth:
		out.Reason = PoolReasonAuth
		out.Status = http.StatusServiceUnavailable
	case scheduler.SelectionValidating:
		out.Reason = PoolReasonValidating
		out.Status = http.StatusServiceUnavailable
	default:
		out.Reason = PoolReasonEmpty
		out.Status = http.StatusServiceUnavailable
	}
	return out
}
