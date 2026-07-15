package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// ClientModelAuthorizationMiddleware enforces the authenticated key's model
// policy while leaving the request body available to the protocol bridge.
func ClientModelAuthorizationMiddleware(defaultModel string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 32<<20))
		if err != nil {
			writeOpenAIErrorCode(writer, http.StatusBadRequest, "invalid_request", "Invalid request body")
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var envelope struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			// Preserve the existing protocol handler's detailed invalid-JSON error.
			next.ServeHTTP(writer, request)
			return
		}
		grant, _ := service.ClientGrantFromContext(request.Context())
		model, err := grant.AuthorizeModel(envelope.Model, defaultModel)
		if errors.Is(err, service.ErrModelNotAllowed) {
			writeOpenAIErrorCode(writer, http.StatusForbidden, "model_not_allowed", "Model is not allowed for this API key")
			return
		}
		if err != nil {
			writeOpenAIErrorCode(writer, http.StatusInternalServerError, "authorization_failed", "Model authorization failed")
			return
		}
		next.ServeHTTP(writer, request.WithContext(service.WithEffectiveModel(request.Context(), model)))
	})
}

// ClientModelsScopeMiddleware filters a successful OpenAI model-list response
// to the caller's effective model scope.
func ClientModelsScopeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		buffered := &bufferedHTTPResponse{header: make(http.Header), status: http.StatusOK}
		next.ServeHTTP(buffered, request)
		body := buffered.body.Bytes()
		if buffered.status == http.StatusOK {
			grant, _ := service.ClientGrantFromContext(request.Context())
			body = filterModelsPayload(body, grant)
		}
		copyHTTPHeader(writer.Header(), buffered.header)
		writer.Header().Del("Content-Length")
		writer.WriteHeader(buffered.status)
		_, _ = writer.Write(body)
	})
}

func filterModelsPayload(body []byte, grant service.ClientGrant) []byte {
	if !grant.Authenticated || grant.ModelPolicy == "all" {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	raw, ok := payload["data"].([]any)
	if !ok {
		return body
	}
	filtered := make([]any, 0, len(raw))
	for _, value := range raw {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		model, _ := item["id"].(string)
		if strings.TrimSpace(model) == "" {
			model, _ = item["model"].(string)
		}
		if grant.AllowsModel(model) {
			filtered = append(filtered, value)
		}
	}
	payload["data"] = filtered
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return append(encoded, '\n')
}

type bufferedHTTPResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *bufferedHTTPResponse) Header() http.Header { return r.header }

func (r *bufferedHTTPResponse) WriteHeader(status int) {
	if status > 0 {
		r.status = status
	}
}

func (r *bufferedHTTPResponse) Write(payload []byte) (int, error) {
	return r.body.Write(payload)
}

func copyHTTPHeader(destination, source http.Header) {
	for name, values := range source {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}
