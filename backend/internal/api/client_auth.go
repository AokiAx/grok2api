package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/AokiAx/grok2api/backend/internal/service"
)

type ClientAuthenticator interface {
	Authenticate(context.Context, string) (service.ClientGrant, error)
}

// ClientAuthMiddleware authenticates a request and attaches only the opaque
// client-key principal to its context. Bearer credentials take precedence over
// x-api-key when both are present.
func ClientAuthMiddleware(authenticator ClientAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if authenticator == nil {
			writeOpenAIErrorCode(writer, http.StatusInternalServerError, "authentication_unavailable", "Client authentication is unavailable")
			return
		}
		grant, err := authenticator.Authenticate(request.Context(), clientSecretFromRequest(request))
		if errors.Is(err, service.ErrClientUnauthorized) {
			writeOpenAIErrorCode(writer, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
			return
		}
		if err != nil {
			writeOpenAIErrorCode(writer, http.StatusInternalServerError, "authentication_failed", "Client authentication failed")
			return
		}
		next.ServeHTTP(writer, request.WithContext(service.WithClientGrant(request.Context(), grant)))
	})
}

func clientSecretFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	authorization := strings.TrimSpace(request.Header.Get("Authorization"))
	if authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	return strings.TrimSpace(request.Header.Get("x-api-key"))
}
