package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/requestctx"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

type ClientAuthenticator interface {
	Authenticate(context.Context, string) (service.ClientGrant, error)
}

const maxInferenceBodyBytes = 32 << 20

type inferenceRequestBodyContextKey struct{}

func withInferenceRequestBody(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, inferenceRequestBodyContextKey{}, body)
}

func inferenceRequestBodyFromContext(ctx context.Context) ([]byte, bool) {
	if ctx == nil {
		return nil, false
	}
	body, ok := ctx.Value(inferenceRequestBodyContextKey{}).([]byte)
	return body, ok
}

func readInferenceRequestBody(_ http.ResponseWriter, request *http.Request) ([]byte, error) {
	if request == nil {
		return nil, errors.New("inference request is required")
	}
	if body, ok := inferenceRequestBodyFromContext(request.Context()); ok {
		return body, nil
	}
	if cache := requestctx.BodyCacheFromContext(request.Context()); cache != nil {
		return cache.Load(func() ([]byte, error) {
			return requestctx.ReadBounded(request.Body, maxInferenceBodyBytes)
		})
	}
	return requestctx.ReadBounded(request.Body, maxInferenceBodyBytes)
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
		body, err := readInferenceRequestBody(writer, request)
		if err != nil {
			writeOpenAIErrorCode(writer, http.StatusBadRequest, "invalid_request", "Invalid request body")
			return
		}
		request = request.WithContext(withInferenceRequestBody(request.Context(), body))
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

type ClientRateConsumer interface {
	ConsumeRPM(context.Context, service.ClientGrant) (repository.RateLimitDecision, error)
}

// ClientRateLimitMiddleware consumes one persisted per-key RPM unit. SQLite is
// authoritative for both the configured limit and the atomic window state.
func ClientRateLimitMiddleware(consumer ClientRateConsumer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if consumer == nil {
			writeOpenAIErrorCode(writer, http.StatusInternalServerError, "rate_limit_unavailable", "Rate limiting is unavailable")
			return
		}
		grant, _ := service.ClientGrantFromContext(request.Context())
		decision, err := consumer.ConsumeRPM(request.Context(), grant)
		writeRateLimitHeaders(writer.Header(), decision)
		if errors.Is(err, service.ErrClientRateLimited) {
			writeOpenAIErrorCode(writer, http.StatusTooManyRequests, "rate_limit_exceeded", "Rate limit exceeded for this API key")
			return
		}
		if err != nil {
			writeOpenAIErrorCode(writer, http.StatusInternalServerError, "rate_limit_failed", "Rate limit check failed")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func writeRateLimitHeaders(header http.Header, decision repository.RateLimitDecision) {
	if decision.Limit > 0 {
		header.Set("X-RateLimit-Limit-Requests", strconv.Itoa(decision.Limit))
		header.Set("X-RateLimit-Remaining-Requests", strconv.Itoa(decision.Remaining))
	}
	if !decision.ResetAt.IsZero() {
		header.Set("X-RateLimit-Reset-Requests", strconv.FormatInt(decision.ResetAt.UTC().Unix(), 10))
	}
}

type ClientConcurrencyLimiter interface {
	AcquireConcurrency(service.ClientGrant) (service.ClientPermit, error)
}

type ClientInferenceAccess interface {
	ClientAuthenticator
	ClientRateConsumer
	ClientConcurrencyLimiter
}

// ClientInferenceMiddleware fixes the security order for request-bearing
// inference endpoints: authenticate, consume persisted RPM, hold a per-key
// concurrency permit, then read and authorize one bounded body. The permit is
// retained while the request uploads, parses, and streams its full response.
func ClientInferenceMiddleware(access ClientInferenceAccess, defaultModel string, next http.Handler) http.Handler {
	return ClientAuthMiddleware(
		access,
		ClientRateLimitMiddleware(
			access,
			ClientConcurrencyMiddleware(
				access,
				ClientModelAuthorizationMiddleware(defaultModel, next),
			),
		),
	)
}

// ClientModelsMiddleware authenticates the model-list request and filters the
// response by scope without charging inference RPM or concurrency.
func ClientModelsMiddleware(authenticator ClientAuthenticator, next http.Handler) http.Handler {
	return ClientAuthMiddleware(authenticator, ClientModelsScopeMiddleware(next))
}

// ClientConcurrencyMiddleware holds the permit until the wrapped handler has
// fully returned. Since the existing streaming handlers return only after the
// response stream reaches EOF or disconnects, streaming permits cover the full
// response lifetime.
func ClientConcurrencyMiddleware(limiter ClientConcurrencyLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if limiter == nil {
			writeOpenAIErrorCode(writer, http.StatusInternalServerError, "concurrency_unavailable", "Concurrency limiting is unavailable")
			return
		}
		grant, _ := service.ClientGrantFromContext(request.Context())
		permit, err := limiter.AcquireConcurrency(grant)
		if errors.Is(err, service.ErrClientConcurrencyLimited) {
			writeOpenAIErrorCode(writer, http.StatusTooManyRequests, "concurrent_limit_exceeded", "Concurrent request limit exceeded for this API key")
			return
		}
		if err != nil {
			writeOpenAIErrorCode(writer, http.StatusInternalServerError, "concurrency_failed", "Concurrency check failed")
			return
		}
		defer permit.Release()
		next.ServeHTTP(writer, request)
	})
}
