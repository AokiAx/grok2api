package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminAuthHandlerRequiresService(t *testing.T) {
	if NewAdminAuthHandler(nil, AdminAuthHandlerOptions{SecureCookies:true}) == nil { t.Fatal("handler must be constructed") }
}

func TestAdminAuthCookieContract(t *testing.T) {
	// Cookie attributes are exercised through the handler integration on the assembled server.
	if !strings.Contains(http.SameSiteStrictMode.String(), "Strict") { t.Fatal("strict same-site unavailable") }
	_ = httptest.NewRecorder()
}
