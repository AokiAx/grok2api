package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/domain/audit"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/requestctx"
	"github.com/AokiAx/grok2api/backend/internal/scheduler"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

type Upstream interface {
	Request(context.Context, account.Account, string, string, []byte, bool) (*http.Response, error)
}

type ChatResult struct {
	Status int
	Header http.Header
	Body   []byte
	Stream io.ReadCloser
	// Optional per-request token usage extracted from upstream bodies when available.
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	TotalTokens       int64
}

// AuditSink records gateway request outcomes without storing payloads/secrets.
type AuditSink interface {
	RecordRequestAudit(context.Context, audit.Request, []audit.Attempt) error
}

type Gateway struct {
	scheduler       *scheduler.Scheduler
	store           repository.AccountSaver
	upstream        Upstream
	auditor         AuditSink
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
	runtimeMu      sync.RWMutex
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

// WithAuditSink enables request audit recording.
func WithAuditSink(sink AuditSink) Option {
	return func(gateway *Gateway) {
		gateway.auditor = sink
	}
}

func NewGateway(
	scheduler *scheduler.Scheduler,
	store repository.AccountSaver,
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
) (result ChatResult, err error) {
	started := g.now().UTC()
	model := modelFromPayload(payload)
	if effective, ok := EffectiveModelFromContext(ctx); ok && effective != "" {
		model = effective
	}
	clientKeyID := ""
	if grant, ok := ClientGrantFromContext(ctx); ok {
		clientKeyID = grant.KeyID
	}
	var attempts []audit.Attempt
	var lastAccountID string
	var statusCode int
	var errorType, errorCode string
	success := false

	defer func() {
		finished := g.now().UTC()
		if g == nil || g.auditor == nil {
			return
		}
		// Never fail the user request because audit persistence failed.
		op := operationFromPath(method, path)
		if statusCode == 0 && err != nil {
			if poolErr, ok := AsPoolUnavailable(err); ok {
				statusCode = poolErr.Status
				errorType = "pool"
				errorCode = string(poolErr.Reason)
			} else {
				statusCode = http.StatusBadGateway
				errorType = "provider"
				errorCode = "upstream_error"
			}
		}
		if result.Status > 0 {
			statusCode = result.Status
		}
		if statusCode > 0 && statusCode < 400 {
			success = true
			errorType = ""
			errorCode = ""
		}
		reqID := requestctx.RequestID(ctx)
		id := newAuditID()
		item := audit.Request{
			ID:                id,
			RequestID:         reqID,
			StartedAt:         started,
			FinishedAt:        finished,
			DurationMS:        finished.Sub(started).Milliseconds(),
			Method:            method,
			Path:              path,
			Operation:         op,
			Model:             model,
			ClientKeyID:       clientKeyID,
			AccountID:         lastAccountID,
			StatusCode:        statusCode,
			Success:           success,
			ErrorType:         errorType,
			ErrorCode:         errorCode,
			InputTokens:       result.InputTokens,
			CachedInputTokens: result.CachedInputTokens,
			OutputTokens:      result.OutputTokens,
			TotalTokens:       result.TotalTokens,
			AttemptCount:      len(attempts),
			Stream:            stream,
		}
		// Prefer body-derived usage. If missing, leave zeros rather than inventing
		// deltas from free-tier cumulative rate-limit headers.
		if item.TotalTokens == 0 && item.InputTokens == 0 && item.OutputTokens == 0 && item.CachedInputTokens == 0 && len(result.Body) > 0 {
			inT, cachedT, outT, totT := extractBodyUsage(result.Body)
			item.InputTokens, item.CachedInputTokens, item.OutputTokens, item.TotalTokens = inT, cachedT, outT, totT
		}
		// Streaming successes are audited when the stream closes (usage sits in the tail).
		if stream && success && result.Stream != nil {
			return
		}
		_ = g.auditor.RecordRequestAudit(context.WithoutCancel(ctx), item, attempts)
	}()

	if circuitError := g.quotaCircuitError(); circuitError != nil {
		return ChatResult{}, circuitError
	}
	ready := g.scheduler.ReadyCount()
	if ready == 0 {
		return ChatResult{}, g.poolUnavailable()
	}
	// Critical: never rotate through the entire ready pool. One exhausted or
	// permission-denied response would otherwise park every credential.
	_, _, acquireTimeout, maxAttempts := g.runtimeSnapshot()
	attemptBudget := maxAttempts
	if attemptBudget <= 0 {
		attemptBudget = 3
	}
	if ready < attemptBudget {
		attemptBudget = ready
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
	for range attemptBudget {
		acquireCtx := ctx
		var cancel context.CancelFunc
		if acquireTimeout > 0 {
			acquireCtx, cancel = context.WithTimeout(ctx, acquireTimeout)
		}
		lease, acquireErr := g.scheduler.AcquireSticky(acquireCtx, stickyKey)
		if cancel != nil {
			cancel()
		}
		if acquireErr != nil {
			if errors.Is(acquireErr, scheduler.ErrNoReadyAccount) {
				return ChatResult{}, g.poolUnavailableFrom(acquireErr)
			}
			if errors.Is(acquireErr, context.DeadlineExceeded) || errors.Is(acquireErr, context.Canceled) {
				return ChatResult{}, g.poolUnavailableFrom(&scheduler.SelectionError{
					Reason:     scheduler.SelectionSaturated,
					RetryAfter: time.Second,
				})
			}
			return ChatResult{}, fmt.Errorf("acquire account: %w", acquireErr)
		}
		attempted++
		attemptStart := g.now().UTC()
		accountID := lease.Account().ID
		lastAccountID = accountID
		response, upErr := g.upstream.Request(
			ctx,
			lease.Account(),
			method,
			path,
			payload,
			stream,
		)
		if upErr != nil {
			attemptEnd := g.now().UTC()
			attempts = append(attempts, audit.Attempt{
				Ordinal:    attempted,
				AccountID:  accountID,
				StartedAt:  attemptStart,
				FinishedAt: attemptEnd,
				DurationMS: attemptEnd.Sub(attemptStart).Milliseconds(),
				StatusCode: 0,
				Success:    false,
				ErrorType:  "provider",
				ErrorCode:  "upstream_transport",
				Rotated:    false,
			})
			lease.Release()
			g.resetCircuit()
			return ChatResult{}, upErr
		}
		if stream && response.StatusCode < 400 {
			// Best-effort: never fail a successful upstream response because SQLite is busy.
			_ = g.persistSuccessUsage(ctx, lease, response.Header)
			g.scheduler.NoteSuccess(accountID)
			// Keep returning this successful stream even if remaining hit 0;
			// the account is already marked unavailable for subsequent traffic.
			g.resetCircuit()
			attemptEnd := g.now().UTC()
			attempts = append(attempts, audit.Attempt{
				Ordinal:    attempted,
				AccountID:  accountID,
				StartedAt:  attemptStart,
				FinishedAt: attemptEnd,
				DurationMS: attemptEnd.Sub(attemptStart).Milliseconds(),
				StatusCode: response.StatusCode,
				Success:    true,
				Rotated:    false,
			})
			statusCode = response.StatusCode
			success = true
			auditItem := audit.Request{
				ID:           newAuditID(),
				RequestID:    requestctx.RequestID(ctx),
				StartedAt:    started,
				Method:       method,
				Path:         path,
				Operation:    operationFromPath(method, path),
				Model:        model,
				ClientKeyID:  clientKeyID,
				AccountID:    accountID,
				StatusCode:   response.StatusCode,
				Success:      true,
				AttemptCount: len(attempts),
				Stream:       true,
			}
			return ChatResult{
				Status: response.StatusCode,
				Header: response.Header.Clone(),
				Stream: &leaseReadCloser{
					body:     response.Body,
					lease:    lease,
					auditor:  g.auditor,
					audit:    auditItem,
					attempts: append([]audit.Attempt(nil), attempts...),
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
			g.scheduler.NoteSuccess(accountID)
			// Return the successful body even if this response exhausted free quota.
			lease.Release()
			g.resetCircuit()
			attemptEnd := g.now().UTC()
			attempts = append(attempts, audit.Attempt{
				Ordinal:    attempted,
				AccountID:  accountID,
				StartedAt:  attemptStart,
				FinishedAt: attemptEnd,
				DurationMS: attemptEnd.Sub(attemptStart).Milliseconds(),
				StatusCode: response.StatusCode,
				Success:    true,
				Rotated:    false,
			})
			statusCode = response.StatusCode
			success = true
			inT, cachedT, outT, totT := extractBodyUsage(body)
			return ChatResult{
				Status:            response.StatusCode,
				Header:            response.Header.Clone(),
				Body:              body,
				InputTokens:       inT,
				CachedInputTokens: cachedT,
				OutputTokens:      outT,
				TotalTokens:       totT,
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
			streak := g.scheduler.NoteFailure(accountID)
			retryAt := g.retryAtFor(failure, response.Header, streak)
			reason := failure.Reason
			if reason == "" {
				reason = account.ReasonCooldown
			}
			// MoveUnavailable also clears sticky bindings for this account.
			lease.MoveUnavailable(reason, retryAt, failure.Code)
			updated := lease.Account()
			if failure.QuotaLimit > 0 || failure.QuotaActual > 0 {
				updated.SetQuota(failure.QuotaActual, failure.QuotaLimit)
			}
			if saveErr := g.store.SaveAccount(ctx, updated); saveErr != nil {
				lease.Release()
				return ChatResult{}, fmt.Errorf("save account transition: %w", saveErr)
			}
			lease.Release()
			attemptEnd := g.now().UTC()
			attempts = append(attempts, audit.Attempt{
				Ordinal:    attempted,
				AccountID:  accountID,
				StartedAt:  attemptStart,
				FinishedAt: attemptEnd,
				DurationMS: attemptEnd.Sub(attemptStart).Milliseconds(),
				StatusCode: response.StatusCode,
				Success:    false,
				ErrorType:  string(failure.Kind),
				ErrorCode:  failure.Code,
				Rotated:    true,
			})
			errorType = string(failure.Kind)
			errorCode = failure.Code
			statusCode = response.StatusCode
			continue
		}
		lease.Release()
		g.resetCircuit()
		attemptEnd := g.now().UTC()
		attempts = append(attempts, audit.Attempt{
			Ordinal:    attempted,
			AccountID:  accountID,
			StartedAt:  attemptStart,
			FinishedAt: attemptEnd,
			DurationMS: attemptEnd.Sub(attemptStart).Milliseconds(),
			StatusCode: response.StatusCode,
			Success:    false,
			ErrorType:  string(failure.Kind),
			ErrorCode:  failure.Code,
			Rotated:    false,
		})
		statusCode = response.StatusCode
		errorType = string(failure.Kind)
		errorCode = failure.Code
		return ChatResult{
			Status: response.StatusCode,
			Header: response.Header.Clone(),
			Body:   body,
		}, nil
	}
	if attempted > 0 && quotaFailures == attempted {
		g.openQuotaCircuit()
		if circuitErr := g.quotaCircuitError(); circuitErr != nil {
			return ChatResult{}, circuitErr
		}
	}

	return ChatResult{}, g.poolUnavailable()
}

func modelFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	// Tiny scan without full JSON dependency cycles.
	// Prefer "model":"..." pattern.
	const key = `"model"`
	idx := strings.Index(string(payload), key)
	if idx < 0 {
		return ""
	}
	rest := string(payload[idx+len(key):])
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, ":") {
		return ""
	}
	rest = strings.TrimSpace(rest[1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func operationFromPath(method, path string) string {
	path = strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.Contains(path, "chat/completions"):
		return "chat"
	case strings.Contains(path, "/responses"):
		return "responses"
	case strings.Contains(path, "/messages"):
		return "messages"
	case strings.Contains(path, "/models"):
		return "models"
	case strings.Contains(path, "/billing"):
		return "billing"
	default:
		return strings.ToLower(method) + " " + path
	}
}

func newAuditID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("aud_%d", time.Now().UnixNano())
	}
	return "aud_" + hex.EncodeToString(raw[:])
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
	lease.RecordUsage(item.QuotaActual, item.QuotaLimit, now)
	item = lease.Account()
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

func (g *Gateway) retryAtFor(failure upstream.Failure, header http.Header, streak int) time.Time {
	now := g.now()
	base := g.rateRetry
	switch {
	case failure.Kind == upstream.FailureQuota:
		// Free-tier windows are long; do not shrink via streak.
		base = g.quotaRetry
		if base <= 0 {
			base = 24 * time.Hour
		}
	case failure.Reason == account.ReasonValidating:
		base = g.validatingRetry
		if base <= 0 {
			base = 45 * time.Second
		}
		base = scheduler.CooldownForStreak(base, streak)
	default:
		if base <= 0 {
			base = 45 * time.Second
		}
		base = scheduler.CooldownForStreak(base, streak)
	}
	if header != nil {
		if fromHeader := upstream.ParseRetryAfter(header.Get("Retry-After"), now); fromHeader > base {
			base = fromHeader
		}
	}
	return now.Add(base)
}

func (g *Gateway) refreshCircuitLocked() {
	if !g.circuit.Open {
		return
	}
	if g.circuit.Revision != g.scheduler.Revision() || !g.circuit.RetryAt.After(g.now()) {
		g.circuit = CircuitStatus{}
	}
}

const streamUsageTailWindow = 96 << 10

type leaseReadCloser struct {
	body     io.ReadCloser
	lease    *scheduler.Lease
	once     sync.Once
	auditor  AuditSink
	audit    audit.Request
	attempts []audit.Attempt
	tail     []byte
}

func (r *leaseReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.body.Read(buffer)
	if count > 0 {
		r.tail = append(r.tail, buffer[:count]...)
		if len(r.tail) > streamUsageTailWindow {
			r.tail = append([]byte(nil), r.tail[len(r.tail)-streamUsageTailWindow:]...)
		}
	}
	if errors.Is(err, io.EOF) {
		_ = r.Close()
	}
	return count, err
}

func (r *leaseReadCloser) Close() error {
	var closeErr error
	r.once.Do(func() {
		closeErr = r.body.Close()
		if r.lease != nil {
			r.lease.Release()
		}
		if r.auditor == nil || strings.TrimSpace(r.audit.ID) == "" {
			return
		}
		item := r.audit
		finished := time.Now().UTC()
		item.FinishedAt = finished
		if !item.StartedAt.IsZero() {
			item.DurationMS = finished.Sub(item.StartedAt).Milliseconds()
		}
		inT, cachedT, outT, totT := extractBodyUsage(r.tail)
		item.InputTokens = inT
		item.CachedInputTokens = cachedT
		item.OutputTokens = outT
		item.TotalTokens = totT
		_ = r.auditor.RecordRequestAudit(context.Background(), item, r.attempts)
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

// ConfigureRuntime updates selected runtime knobs without rebuilding the gateway.
func (g *Gateway) ConfigureRuntime(quotaRetry, rateRetry, acquireTimeout time.Duration, maxAttempts int) {
	if g == nil {
		return
	}
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	if quotaRetry > 0 {
		g.quotaRetry = quotaRetry
	}
	if rateRetry > 0 {
		g.rateRetry = rateRetry
	}
	if acquireTimeout > 0 {
		g.acquireTimeout = acquireTimeout
	}
	if maxAttempts > 0 {
		g.maxAttempts = maxAttempts
	}
}

func (g *Gateway) runtimeSnapshot() (quotaRetry, rateRetry, acquireTimeout time.Duration, maxAttempts int) {
	if g == nil {
		return 0, 0, 0, 3
	}
	g.runtimeMu.RLock()
	defer g.runtimeMu.RUnlock()
	return g.quotaRetry, g.rateRetry, g.acquireTimeout, g.maxAttempts
}

func extractBodyUsage(raw []byte) (input, cached, output, total int64) {
	if len(raw) == 0 {
		return 0, 0, 0, 0
	}
	// Lightweight scans for OpenAI/Anthropic/Responses style usage fields.
	// Prefer last match (completed event often near the end).
	lastInt := func(key string) int64 {
		needle := []byte("\"" + key + "\"")
		idx := -1
		for i := 0; i+len(needle) < len(raw); i++ {
			if bytes.Equal(raw[i:i+len(needle)], needle) {
				idx = i + len(needle)
			}
		}
		if idx < 0 {
			return 0
		}
		// skip : and spaces
		for idx < len(raw) && (raw[idx] == ':' || raw[idx] == ' ' || raw[idx] == '\t') {
			idx++
		}
		start := idx
		for idx < len(raw) && raw[idx] >= '0' && raw[idx] <= '9' {
			idx++
		}
		if start == idx {
			return 0
		}
		n, _ := strconv.ParseInt(string(raw[start:idx]), 10, 64)
		return n
	}
	input = lastInt("input_tokens")
	if input == 0 {
		input = lastInt("prompt_tokens")
	}
	// OpenAI/Grok: input_tokens_details.cached_tokens or top-level cached_tokens.
	// Anthropic: cache_read_input_tokens.
	cached = lastInt("cached_tokens")
	if cached == 0 {
		cached = lastInt("cache_read_input_tokens")
	}
	if cached == 0 {
		cached = lastInt("cached_input_tokens")
	}
	output = lastInt("output_tokens")
	if output == 0 {
		output = lastInt("completion_tokens")
	}
	total = lastInt("total_tokens")
	if total == 0 && (input > 0 || output > 0) {
		total = input + output
	}
	return input, cached, output, total
}
