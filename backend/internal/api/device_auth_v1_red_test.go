package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/deviceauth"
	domain "github.com/AokiAx/grok2api/backend/internal/domain/deviceauth"
)

type redDeviceAuthAdmin struct {
	starts int
}

func (a *redDeviceAuthAdmin) Start(context.Context, deviceauth.StartRequest) (domain.Session, error) {
	a.starts++
	return domain.Session{ID: "should-not-start"}, nil
}

func (*redDeviceAuthAdmin) Get(context.Context, string) (domain.Session, bool, error) {
	return domain.Session{}, false, nil
}

func (*redDeviceAuthAdmin) Cancel(context.Context, string) (domain.Session, error) {
	return domain.Session{}, nil
}

func (*redDeviceAuthAdmin) PollOnce(context.Context, string) (domain.Session, error) {
	return domain.Session{}, nil
}

func TestAdminV1DeviceAuthStartRejectsMalformedJSON(t *testing.T) {
	admin := &redDeviceAuthAdmin{}
	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"",
		api.WithAdmin(&fakeAdmin{}, "panel-secret"),
		api.WithDeviceAuth(admin),
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/v1/device-auth/sessions",
		strings.NewReader(`{"issuer":`),
	)
	request.Header.Set("Authorization", "Bearer panel-secret")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"invalid_json"`) {
		t.Fatalf("body=%s, want invalid_json error", recorder.Body.String())
	}
	if admin.starts != 0 {
		t.Fatalf("Start called %d times for malformed JSON", admin.starts)
	}
}
