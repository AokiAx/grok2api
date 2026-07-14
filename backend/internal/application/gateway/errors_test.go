package gateway

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestSelectionReasonValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  SelectionReason
		want string
	}{
		{name: "no ready", got: SelectionNoReady, want: "no_ready"},
		{name: "saturated", got: SelectionSaturated, want: "saturated"},
		{name: "quota", got: SelectionQuota, want: "quota"},
		{name: "cooling", got: SelectionCooling, want: "cooling"},
		{name: "auth", got: SelectionAuth, want: "auth"},
		{name: "validating", got: SelectionValidating, want: "validating"},
		{name: "quota circuit", got: SelectionQuotaCircuit, want: "quota_circuit"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := string(test.got); got != test.want {
				t.Fatalf("selection reason = %q; want %q", got, test.want)
			}
		})
	}
}

func TestSelectionErrorSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason SelectionReason
		want   string
	}{
		{name: "empty", reason: SelectionNoReady, want: "no ready account"},
		{name: "saturated", reason: SelectionSaturated, want: "ready accounts are at concurrency capacity"},
		{name: "quota", reason: SelectionQuota, want: "ready account pool exhausted by quota"},
		{name: "cooling", reason: SelectionCooling, want: "ready account pool cooling down"},
		{name: "auth", reason: SelectionAuth, want: "ready account pool blocked by auth failures"},
		{name: "validating", reason: SelectionValidating, want: "ready account pool validating"},
		{name: "circuit", reason: SelectionQuotaCircuit, want: "quota circuit open; retry later"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := &SelectionError{Reason: test.reason, RetryAfter: time.Minute, Ready: 1, Unavailable: 2}
			if got := err.Error(); got != test.want {
				t.Fatalf("Error() = %q; want %q", got, test.want)
			}
			if !errors.Is(err, ErrNoReadyAccount) {
				t.Fatalf("errors.Is(%v, ErrNoReadyAccount) = false", err)
			}
			wrapped := fmt.Errorf("acquire account: %w", err)
			got, ok := AsSelectionError(wrapped)
			if !ok || got != err {
				t.Fatalf("AsSelectionError() = (%v, %v); want original selection error", got, ok)
			}
		})
	}
}

func TestNilSelectionErrorHasStableMessage(t *testing.T) {
	t.Parallel()

	var err *SelectionError
	if got := err.Error(); got != "no ready account" {
		t.Fatalf("nil SelectionError.Error() = %q; want no ready account", got)
	}
}

func TestPoolUnavailableErrorSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason SelectionReason
		want   string
	}{
		{name: "empty", reason: SelectionNoReady, want: "ready account pool is empty"},
		{name: "saturated", reason: SelectionSaturated, want: "ready accounts are at concurrency capacity"},
		{name: "quota", reason: SelectionQuota, want: "no ready accounts (quota exhausted)"},
		{name: "cooling", reason: SelectionCooling, want: "no ready accounts (cooling down)"},
		{name: "auth", reason: SelectionAuth, want: "no ready accounts (auth failures)"},
		{name: "validating", reason: SelectionValidating, want: "no ready accounts (validating)"},
		{name: "circuit", reason: SelectionQuotaCircuit, want: "quota circuit open; retry later"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := &PoolUnavailableError{Reason: test.reason, RetryAfter: time.Minute}
			if got := err.Error(); got != test.want {
				t.Fatalf("Error() = %q; want %q", got, test.want)
			}
			if !errors.Is(err, ErrNoReadyAccount) {
				t.Fatalf("errors.Is(%v, ErrNoReadyAccount) = false", err)
			}
			wrapped := fmt.Errorf("gateway request: %w", err)
			got, ok := AsPoolUnavailable(wrapped)
			if !ok || got != err {
				t.Fatalf("AsPoolUnavailable() = (%v, %v); want original pool error", got, ok)
			}
		})
	}
}

func TestPoolUnavailableErrorUsesCustomMessage(t *testing.T) {
	t.Parallel()

	err := &PoolUnavailableError{Reason: SelectionQuota, Message: "capacity is unavailable"}
	if got := err.Error(); got != err.Message {
		t.Fatalf("Error() = %q; want %q", got, err.Message)
	}
}

func TestPoolUnavailableErrorIsTransportNeutral(t *testing.T) {
	t.Parallel()

	typeOfError := reflect.TypeOf(PoolUnavailableError{})
	if _, ok := typeOfError.FieldByName("Status"); ok {
		t.Fatal("PoolUnavailableError must not carry an HTTP status")
	}
}
