package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type contractPool struct{}

func (contractPool) Acquire(context.Context, string) (Lease, error) {
	return contractLease{}, nil
}

func (contractPool) Snapshot() PoolSnapshot {
	return PoolSnapshot{}
}

type contractLease struct{}

func (contractLease) Account() account.Account {
	return account.Account{}
}

func (contractLease) MoveUnavailable(account.UnavailableReason, time.Time, string) {}

func (contractLease) RecordUsage(int64, int64, time.Time) {}

func (contractLease) Release() {}

type contractProvider struct{}

func (contractProvider) Do(context.Context, account.Account, Request) (Response, error) {
	return Response{}, nil
}

type contractStore struct{}

func (contractStore) SaveAccount(context.Context, account.Account) error {
	return nil
}

var (
	_ AccountPool             = contractPool{}
	_ Lease                   = contractLease{}
	_ Provider                = contractProvider{}
	_ AccountStore            = contractStore{}
	_ repository.AccountSaver = contractStore{}
)

func TestOperationValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  Operation
		want string
	}{
		{name: "responses", got: OperationResponses, want: "responses"},
		{name: "chat", got: OperationChat, want: "chat"},
		{name: "models", got: OperationModels, want: "models"},
		{name: "billing", got: OperationBilling, want: "billing"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := string(test.got); got != test.want {
				t.Fatalf("operation = %q; want %q", got, test.want)
			}
		})
	}
}

func TestFailureKindValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  FailureKind
		want string
	}{
		{name: "quota", got: FailureQuota, want: "quota"},
		{name: "rate limit", got: FailureRateLimit, want: "rate_limit"},
		{name: "authentication", got: FailureAuthentication, want: "auth"},
		{name: "authentication alias", got: FailureAuth, want: "auth"},
		{name: "credential pending", got: FailureCredentialPending, want: "credential_pending"},
		{name: "request", got: FailureRequest, want: "request"},
		{name: "provider", got: FailureProvider, want: "provider"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := string(test.got); got != test.want {
				t.Fatalf("failure kind = %q; want %q", got, test.want)
			}
		})
	}
}

func TestContractsCarryApplicationData(t *testing.T) {
	t.Parallel()

	stream := io.NopCloser(strings.NewReader("event"))
	usage := Usage{
		Present:   true,
		Unit:      UsageTokens,
		Consumed:  7,
		Limit:     10,
		Remaining: 3,
	}
	request := Request{
		Operation:      OperationResponses,
		Body:           []byte(`{"model":"grok"}`),
		Stream:         true,
		ConversationID: "conversation-1",
		AffinityKey:    "tenant-1",
	}
	response := Response{
		Status:  429,
		Header:  Header{"X-Trace-ID": {"trace-1"}},
		Body:    []byte(`{"error":"quota"}`),
		Stream:  stream,
		Failure: &Failure{Kind: FailureQuota, Code: "quota", Message: "exhausted", RetryAfter: time.Minute, Usage: usage},
		Usage:   usage,
	}

	if request.ConversationID != "conversation-1" || request.AffinityKey != "tenant-1" {
		t.Fatalf("request metadata was not preserved: %+v", request)
	}
	if response.Failure == nil || response.Failure.Kind != FailureQuota || response.Failure.Usage != usage {
		t.Fatalf("failure contract was not preserved: %+v", response.Failure)
	}
	if response.Header["X-Trace-ID"][0] != "trace-1" || response.Stream != stream {
		t.Fatalf("response transport data was not preserved: %+v", response)
	}
}

func TestPoolSnapshotCarriesSelectionState(t *testing.T) {
	t.Parallel()

	retryAt := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	snapshot := PoolSnapshot{
		Ready:         2,
		Unavailable:   3,
		Reasons:       map[account.UnavailableReason]int{account.ReasonQuota: 2, account.ReasonAuth: 1},
		EarliestRetry: retryAt,
		Revision:      9,
	}

	if snapshot.Ready != 2 || snapshot.Unavailable != 3 || snapshot.Revision != 9 {
		t.Fatalf("pool counts/revision = %+v", snapshot)
	}
	if snapshot.Reasons[account.ReasonQuota] != 2 || !snapshot.EarliestRetry.Equal(retryAt) {
		t.Fatalf("pool reasons/retry = %+v", snapshot)
	}
}

func TestUsageUnitValues(t *testing.T) {
	t.Parallel()

	if UsageTokens != "tokens" {
		t.Fatalf("token usage unit = %q; want tokens", UsageTokens)
	}
	if UsageRequests != "requests" {
		t.Fatalf("request usage unit = %q; want requests", UsageRequests)
	}
}
