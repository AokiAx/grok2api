package service_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/domain/audit"
	"github.com/AokiAx/grok2api/backend/internal/scheduler"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

type fakeStore struct {
	saved []account.Account
}

func (s *fakeStore) SaveAccount(_ context.Context, item account.Account) error {
	s.saved = append(s.saved, item)
	return nil
}

type fakeUpstream struct {
	responses map[string][]*http.Response
	err       error
	method    string
	path      string
	payload   []byte
	stream    bool
}

func (u *fakeUpstream) Request(
	_ context.Context,
	item account.Account,
	method string,
	path string,
	payload []byte,
	stream bool,
) (*http.Response, error) {
	u.method = method
	u.path = path
	u.payload = append([]byte(nil), payload...)
	u.stream = stream
	if u.err != nil {
		return nil, u.err
	}
	queue := u.responses[item.ID]
	response := queue[0]
	u.responses[item.ID] = queue[1:]
	return response, nil
}

func response(status int, body string) *http.Response {
	return responseWithHeaders(status, body, nil)
}

func responseWithHeaders(status int, body string, headers map[string]string) *http.Response {
	header := make(http.Header)
	for key, value := range headers {
		header.Set(key, value)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func ready(id string) account.Account {
	return account.Account{
		ID:          id,
		AccessToken: "token-" + id,
		Pool:        account.PoolReady,
		MaxActive:   1,
	}
}

func TestQuotaAccountMovesUnavailableAndNextReadyAccountSucceeds(t *testing.T) {
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"subscription:free-usage-exhausted","error":"rolling 24-hour window"}`)},
		"b": {response(200, `{"choices":[{"message":{"content":"ok"}}]}`)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a"), ready("b")}),
		store,
		upstream,
		service.WithQuotaRetry(30*time.Minute),
	)

	got, err := gateway.Chat(context.Background(), []byte(`{"stream":false}`), false)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got.Status != http.StatusOK || string(got.Body) != `{"choices":[{"message":{"content":"ok"}}]}` {
		t.Fatalf("response = %d %s", got.Status, got.Body)
	}
	if len(store.saved) < 1 {
		t.Fatalf("saved transitions = %d; want >=1", len(store.saved))
	}
	foundQuota := false
	for _, item := range store.saved {
		if item.UnavailableReason == account.ReasonQuota {
			foundQuota = true
		}
	}
	if !foundQuota {
		t.Fatalf("saved = %#v; want quota transition", store.saved)
	}
}

func TestAllQuotaReturnsStructuredPoolUnavailableError(t *testing.T) {
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		store,
		upstream,
		service.WithQuotaRetry(45*time.Minute),
	)

	_, err := gateway.Chat(context.Background(), []byte(`{"stream":false}`), false)
	poolErr, ok := service.AsPoolUnavailable(err)
	if !ok {
		t.Fatalf("error = %v; want pool unavailable", err)
	}
	if poolErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d; want 429", poolErr.Status)
	}
	if poolErr.RetryAfter <= 0 {
		t.Fatalf("retry after = %s", poolErr.RetryAfter)
	}
}

func TestStreamingResponseKeepsLeaseUntilStreamCloses(t *testing.T) {
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(200, "data: hello\n\ndata: [DONE]\n\n")},
	}}
	s := scheduler.New([]account.Account{ready("a")})
	gateway := service.NewGateway(s, store, upstream)

	got, err := gateway.Chat(context.Background(), []byte(`{"stream":true}`), true)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got.Stream == nil {
		t.Fatal("stream response missing")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Acquire(canceled); err == nil {
		t.Fatal("lease was released before stream close")
	}
	data, err := io.ReadAll(got.Stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if err := got.Stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	if string(data) != "data: hello\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream = %q", data)
	}
	next, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after close: %v", err)
	}
	next.Release()
}

func TestOrdinaryRateLimitMovesAccountToCooldown(t *testing.T) {
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"rate-limit"}`)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		store,
		upstream,
		service.WithRateRetry(15*time.Second),
	)

	_, err := gateway.Chat(context.Background(), []byte(`{}`), false)
	if _, ok := service.AsPoolUnavailable(err); !ok {
		t.Fatalf("error = %v; want pool unavailable", err)
	}
	if len(store.saved) != 1 || store.saved[0].UnavailableReason != account.ReasonCooldown {
		t.Fatalf("saved = %#v", store.saved)
	}
}

func TestUpstreamNetworkErrorIsReturned(t *testing.T) {
	want := context.DeadlineExceeded
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		&fakeStore{},
		&fakeUpstream{err: want},
	)
	_, err := gateway.Chat(context.Background(), []byte(`{}`), false)
	if err == nil {
		t.Fatal("expected network error")
	}
}

func TestBadRequestIsReturnedWithoutMovingAccount(t *testing.T) {
	store := &fakeStore{}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		store,
		&fakeUpstream{responses: map[string][]*http.Response{
			"a": {response(422, `{"error":"invalid request"}`)},
		}},
	)
	result, err := gateway.Chat(context.Background(), []byte(`{}`), false)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if result.Status != 422 || len(store.saved) != 0 {
		t.Fatalf("result = %d, saved=%d", result.Status, len(store.saved))
	}
}

func TestPermissionDeniedRotatesToNextAccount(t *testing.T) {
	// Regression: classifying post-mint 403 permission-denied as validating
	// must not leak 403 to clients — rotate and succeed on the next account.
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(403, `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`)},
		"b": {response(200, `{"choices":[{"message":{"content":"ok"}}]}`)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a"), ready("b")}),
		store,
		upstream,
		service.WithValidatingRetry(30*time.Second),
	)

	got, err := gateway.Chat(context.Background(), []byte(`{"stream":false}`), false)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got.Status != http.StatusOK || string(got.Body) != `{"choices":[{"message":{"content":"ok"}}]}` {
		t.Fatalf("response = %d %s", got.Status, got.Body)
	}
	if len(store.saved) < 1 {
		t.Fatalf("expected validating park, saved=%d", len(store.saved))
	}
	parked := store.saved[0]
	if parked.UnavailableReason != account.ReasonValidating || parked.LastErrorCode != "permission-denied" {
		t.Fatalf("parked = %#v", parked)
	}
}

func TestMaxAttemptsDoesNotBurnEntireReadyPool(t *testing.T) {
	// Regression: attempts used to equal ReadyCount(), so one bad request
	// parked every credential. Cap must leave remaining accounts ready.
	store := &fakeStore{}
	up := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
		"b": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
		"c": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
		"d": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
		"e": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
	}}
	pool := scheduler.New([]account.Account{
		ready("a"), ready("b"), ready("c"), ready("d"), ready("e"),
	})
	gateway := service.NewGateway(
		pool,
		store,
		up,
		service.WithMaxAttempts(2),
		service.WithQuotaRetry(time.Hour),
	)
	_, err := gateway.Chat(context.Background(), []byte(`{}`), false)
	if _, ok := service.AsPoolUnavailable(err); !ok {
		t.Fatalf("error=%v; want pool unavailable after attempt budget", err)
	}
	if pool.ReadyCount() != 3 {
		t.Fatalf("ready=%d; want 3 remaining after max_attempts=2", pool.ReadyCount())
	}
	if len(store.saved) != 2 {
		t.Fatalf("parked=%d; want 2", len(store.saved))
	}
}

func TestPermissionDeniedAloneReturnsPoolUnavailable(t *testing.T) {
	store := &fakeStore{}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		store,
		&fakeUpstream{responses: map[string][]*http.Response{
			"a": {response(403, `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`)},
		}},
	)
	_, err := gateway.Chat(context.Background(), []byte(`{}`), false)
	if _, ok := service.AsPoolUnavailable(err); !ok {
		t.Fatalf("error = %v; want pool unavailable, not raw 403", err)
	}
	if len(store.saved) != 1 || store.saved[0].UnavailableReason != account.ReasonValidating {
		t.Fatalf("saved = %#v", store.saved)
	}
}

func TestGenericRequestUsesSameRotationAndLeasePipeline(t *testing.T) {
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
		"b": {response(200, `{"data":[{"id":"grok-4.5"}]}`)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a"), ready("b")}),
		store,
		upstream,
	)

	result, err := gateway.Request(context.Background(), http.MethodGet, "/models", nil, false)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if result.Status != http.StatusOK || upstream.method != http.MethodGet || upstream.path != "/models" {
		t.Fatalf("result=%d upstream=%s %s", result.Status, upstream.method, upstream.path)
	}
	foundQuota := false
	for _, item := range store.saved {
		if item.UnavailableReason == account.ReasonQuota {
			foundQuota = true
		}
	}
	if !foundQuota {
		t.Fatalf("saved = %#v", store.saved)
	}
}

func TestAllQuotaOpensPoolCircuitUntilSchedulerChanges(t *testing.T) {
	now := time.Now().UTC()
	s := scheduler.New([]account.Account{ready("a"), ready("b")})
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
		"b": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
	}}
	gateway := service.NewGateway(s, &fakeStore{}, upstream, service.WithQuotaRetry(time.Hour))

	if _, err := gateway.Chat(context.Background(), []byte(`{}`), false); err == nil {
		t.Fatal("all quota should fail")
	}
	status := gateway.CircuitStatus()
	if !status.Open || status.RetryAt.Before(now.Add(50*time.Minute)) {
		t.Fatalf("circuit = %#v", status)
	}

	upstream.responses["c"] = []*http.Response{response(200, `{"ok":true}`)}
	s.Upsert(ready("c"))
	result, err := gateway.Chat(context.Background(), []byte(`{}`), false)
	if err != nil || result.Status != http.StatusOK {
		t.Fatalf("request after scheduler change = %d %v", result.Status, err)
	}
	if gateway.CircuitStatus().Open {
		t.Fatal("successful probe should close circuit")
	}
}

func TestMixedFailuresDoNotOpenQuotaCircuit(t *testing.T) {
	s := scheduler.New([]account.Account{ready("a"), ready("b")})
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
		"b": {response(401, `{"code":"invalid-token"}`)},
	}}
	gateway := service.NewGateway(s, &fakeStore{}, upstream)
	_, _ = gateway.Chat(context.Background(), []byte(`{}`), false)
	if gateway.CircuitStatus().Open {
		t.Fatal("mixed quota/auth failures must not open quota circuit")
	}
}

func TestSuccessfulChatPersistsFreeRateLimitUsage(t *testing.T) {
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {responseWithHeaders(200, `{"choices":[{"message":{"content":"ok"}}]}`, map[string]string{
			"x-ratelimit-limit-tokens":       "1000000",
			"x-ratelimit-remaining-tokens":   "750000",
			"x-ratelimit-limit-requests":     "21",
			"x-ratelimit-remaining-requests": "20",
		})},
	}}
	gateway := service.NewGateway(scheduler.New([]account.Account{ready("a")}), store, upstream)

	got, err := gateway.Chat(context.Background(), []byte(`{"stream":false}`), false)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d", got.Status)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved = %d; want 1", len(store.saved))
	}
	item := store.saved[0]
	if item.QuotaLimit != 1_000_000 || item.QuotaActual != 250_000 {
		t.Fatalf("quota = %d/%d; want 250000/1000000", item.QuotaActual, item.QuotaLimit)
	}
	if item.Pool != account.PoolReady {
		t.Fatalf("pool = %s; want ready", item.Pool)
	}
	if item.LastSuccessAt.IsZero() {
		t.Fatal("last success timestamp missing")
	}
}

func TestSuccessfulChatWithZeroRemainingMovesToQuota(t *testing.T) {
	store := &fakeStore{}
	s := scheduler.New([]account.Account{ready("a"), ready("b")})
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {responseWithHeaders(200, `{"choices":[{"message":{"content":"ok"}}]}`, map[string]string{
			"x-ratelimit-limit-tokens":     "1000000",
			"x-ratelimit-remaining-tokens": "0",
		})},
	}}
	gateway := service.NewGateway(
		s,
		store,
		upstream,
		service.WithQuotaRetry(30*time.Minute),
	)

	got, err := gateway.Chat(context.Background(), []byte(`{"stream":false}`), false)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got.Status != http.StatusOK || string(got.Body) != `{"choices":[{"message":{"content":"ok"}}]}` {
		t.Fatalf("should still return successful body, got %d %s", got.Status, got.Body)
	}
	foundExhausted := false
	for _, item := range store.saved {
		if item.ID == "a" && item.UnavailableReason == account.ReasonQuota && item.QuotaActual == 1_000_000 {
			foundExhausted = true
		}
	}
	if !foundExhausted {
		t.Fatalf("saved = %#v; want a moved to quota with full usage", store.saved)
	}
	// next acquire should skip a and use b
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire next: %v", err)
	}
	if lease.Account().ID != "b" {
		t.Fatalf("next account = %s; want b", lease.Account().ID)
	}
	lease.Release()
}

func TestPoolUnavailableErrorString(t *testing.T) {
	err := &service.PoolUnavailableError{Status: 429, RetryAfter: time.Minute}
	if err.Error() == "" {
		t.Fatal("empty error string")
	}
}

func TestPoolUnavailableReasons(t *testing.T) {
	store := &fakeStore{}
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(429, `{"code":"subscription:free-usage-exhausted"}`)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		store,
		upstream,
		service.WithQuotaRetry(45*time.Minute),
	)
	_, err := gateway.Chat(context.Background(), []byte(`{"stream":false}`), false)
	poolErr, ok := service.AsPoolUnavailable(err)
	if !ok {
		t.Fatalf("err=%v", err)
	}
	// After burning the only account on quota, pool is empty/quota (or circuit).
	if poolErr.Reason != service.PoolReasonQuota && poolErr.Reason != service.PoolReasonCircuit {
		t.Fatalf("reason=%q", poolErr.Reason)
	}
	if poolErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status=%d", poolErr.Status)
	}

	// No accounts at all.
	gateway = service.NewGateway(scheduler.New(nil), store, upstream)
	_, err = gateway.Chat(context.Background(), []byte(`{"stream":false}`), false)
	poolErr, ok = service.AsPoolUnavailable(err)
	if !ok || poolErr.Reason != service.PoolReasonEmpty {
		t.Fatalf("empty: ok=%v err=%v reason=%q", ok, err, poolErr)
	}
}

type memoryAuditor struct {
	mu    sync.Mutex
	items []audit.Request
}

func (m *memoryAuditor) RecordRequestAudit(_ context.Context, item audit.Request, _ []audit.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append(m.items, item)
	return nil
}

func TestStreamAuditRecordsCachedUsageOnClose(t *testing.T) {
	store := &fakeStore{}
	auditor := &memoryAuditor{}
	payload := `data: {"type":"response.completed","response":{"usage":{"input_tokens":200,"input_tokens_details":{"cached_tokens":150},"output_tokens":10,"total_tokens":210}}}

data: [DONE]
`
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(200, payload)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		store,
		upstream,
		service.WithAuditSink(auditor),
	)
	got, err := gateway.Request(context.Background(), http.MethodPost, "/responses", []byte(`{"model":"grok-4.5"}`), true)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got.Stream == nil {
		t.Fatal("expected stream")
	}
	if _, err := io.Copy(io.Discard, got.Stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := got.Stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	if len(auditor.items) != 1 {
		t.Fatalf("audits=%d items=%+v", len(auditor.items), auditor.items)
	}
	item := auditor.items[0]
	if item.InputTokens != 200 || item.CachedInputTokens != 150 || item.OutputTokens != 10 || item.TotalTokens != 210 {
		t.Fatalf("tokens=%+v", item)
	}
	if !item.Stream || !item.Success {
		t.Fatalf("flags=%+v", item)
	}
}

func TestNonStreamAuditRecordsCachedUsage(t *testing.T) {
	store := &fakeStore{}
	auditor := &memoryAuditor{}
	body := `{"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":80},"output_tokens":15,"total_tokens":135},"output":[]}`
	upstream := &fakeUpstream{responses: map[string][]*http.Response{
		"a": {response(200, body)},
	}}
	gateway := service.NewGateway(
		scheduler.New([]account.Account{ready("a")}),
		store,
		upstream,
		service.WithAuditSink(auditor),
	)
	got, err := gateway.Request(context.Background(), http.MethodPost, "/responses", []byte(`{"model":"grok-4.5"}`), false)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if got.CachedInputTokens != 80 || got.InputTokens != 120 || got.OutputTokens != 15 || got.TotalTokens != 135 {
		t.Fatalf("result tokens=%+v", got)
	}
	// allow async? non-stream records in defer immediately
	if len(auditor.items) != 1 {
		t.Fatalf("audits=%d", len(auditor.items))
	}
	item := auditor.items[0]
	if item.CachedInputTokens != 80 || item.InputTokens != 120 {
		t.Fatalf("audit=%+v", item)
	}
}
