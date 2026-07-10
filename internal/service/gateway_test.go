package service_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/scheduler"
	"github.com/AokiAx/grok2api/internal/service"
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
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
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
	if len(store.saved) != 1 {
		t.Fatalf("saved transitions = %d; want 1", len(store.saved))
	}
	if store.saved[0].UnavailableReason != account.ReasonQuota {
		t.Fatalf("reason = %q; want quota", store.saved[0].UnavailableReason)
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
	if len(store.saved) != 1 || store.saved[0].UnavailableReason != account.ReasonQuota {
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
