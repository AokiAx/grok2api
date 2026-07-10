package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
	"github.com/AokiAx/grok2api/internal/compat"
	"github.com/AokiAx/grok2api/internal/service"
)

//go:embed panel.html
var panelHTML []byte

type Gateway interface {
	Chat(context.Context, []byte, bool) (service.ChatResult, error)
	Request(context.Context, string, string, []byte, bool) (service.ChatResult, error)
}

type PoolStatus struct {
	Ready       int            `json:"ready"`
	Unavailable int            `json:"unavailable"`
	Reasons     map[string]int `json:"reasons"`
}

type StatusProvider interface {
	PoolStatus() PoolStatus
}

type Server struct {
	gateway      Gateway
	status       StatusProvider
	apiKey       string
	defaultModel string
	admin        AdminService
	adminKey     string
	handler      http.Handler
}

type Option func(*Server)

func WithDefaultModel(model string) Option {
	return func(server *Server) {
		server.defaultModel = model
	}
}

type AdminService interface {
	List(context.Context) ([]account.Account, error)
	Import(context.Context, admin.ImportRequest) (admin.ImportResult, error)
}

func WithAdmin(service AdminService, key string) Option {
	return func(server *Server) {
		server.admin = service
		server.adminKey = key
	}
}

func NewServer(gateway Gateway, status StatusProvider, apiKey string, options ...Option) *Server {
	server := &Server{
		gateway:      gateway,
		status:       status,
		apiKey:       apiKey,
		defaultModel: "grok-4.5",
	}
	for _, option := range options {
		option(server)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.health)
	mux.HandleFunc("GET /v1/models", server.models)
	mux.HandleFunc("GET /v1/billing", server.billing)
	mux.HandleFunc("POST /v1/chat/completions", server.chat)
	mux.HandleFunc("POST /chat/completions", server.chat)
	mux.HandleFunc("POST /v1/responses", server.responses)
	mux.HandleFunc("POST /v1/messages", server.messages)
	mux.HandleFunc("GET /panel", server.panel)
	mux.HandleFunc("GET /manager", server.panel)
	mux.HandleFunc("GET /admin/api/panel-meta", server.panelMeta)
	if server.admin != nil {
		mux.HandleFunc("GET /admin/api/cli-accounts", server.adminList)
		mux.HandleFunc("POST /admin/api/accounts/import/preview", server.adminImportPreview)
		mux.HandleFunc("POST /admin/api/accounts/import", server.adminImport)
	}
	server.handler = mux
	return server
}

func (s *Server) panel(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(panelHTML)
}

func (s *Server) panelMeta(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"auth_required": s.adminKey != "",
		"version":       "1.0.0-go",
	})
}

func (s *Server) adminList(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	accounts, err := s.admin.List(request.Context())
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	public := make([]map[string]any, 0, len(accounts))
	for _, item := range accounts {
		public = append(public, map[string]any{
			"id":                 item.ID,
			"email":              item.Email,
			"pool":               item.Pool,
			"unavailable_reason": item.UnavailableReason,
			"retry_at":           item.RetryAt,
			"last_error_code":    item.LastErrorCode,
			"quota_actual":       item.QuotaActual,
			"quota_limit":        item.QuotaLimit,
			"request_count":      item.RequestCount,
			"active":             item.Active,
			"max_active":         item.MaxActive,
			"has_refresh_token":  item.RefreshToken != "",
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"count":    len(accounts),
		"accounts": public,
	})
}

func (s *Server) adminImportPreview(writer http.ResponseWriter, request *http.Request) {
	s.handleAdminImport(writer, request, true)
}

func (s *Server) adminImport(writer http.ResponseWriter, request *http.Request) {
	s.handleAdminImport(writer, request, false)
}

func (s *Server) handleAdminImport(writer http.ResponseWriter, request *http.Request, dryRun bool) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	var payload admin.ImportRequest
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 32<<20)).Decode(&payload); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid import payload")
		return
	}
	payload.DryRun = dryRun
	result, err := s.admin.Import(request.Context(), payload)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	status := s.status.PoolStatus()
	writeJSON(writer, http.StatusOK, map[string]any{
		"ok":           status.Ready > 0,
		"version":      "1.0.0-go",
		"account_pool": status,
	})
}

func (s *Server) chat(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid API key")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 32<<20))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request body")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if model, ok := payload["model"].(string); !ok || model == "" {
		payload["model"] = s.defaultModel
	}
	stream, _ := payload["stream"].(bool)
	body, err = json.Marshal(payload)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request payload")
		return
	}
	result, err := s.gateway.Chat(request.Context(), body, stream)
	if err != nil {
		s.writeGatewayError(writer, err)
		return
	}
	s.writeResult(writer, result)
}

func (s *Server) models(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid API key")
		return
	}
	result, err := s.gateway.Request(request.Context(), http.MethodGet, "/models", nil, false)
	if err != nil || result.Status >= http.StatusBadRequest {
		writeJSON(writer, http.StatusOK, fallbackModels(s.defaultModel))
		return
	}
	var upstreamPayload map[string]any
	if err := json.Unmarshal(result.Body, &upstreamPayload); err != nil {
		writeJSON(writer, http.StatusOK, fallbackModels(s.defaultModel))
		return
	}
	rawModels, ok := upstreamPayload["data"].([]any)
	if !ok {
		s.writeResult(writer, result)
		return
	}
	now := time.Now().Unix()
	models := make([]map[string]any, 0, len(rawModels))
	for _, raw := range rawModels {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		normalized := make(map[string]any, len(item)+4)
		for key, value := range item {
			normalized[key] = value
		}
		if id, _ := item["id"].(string); id == "" {
			normalized["id"] = item["model"]
		}
		normalized["object"] = "model"
		if _, ok := normalized["created"]; !ok {
			normalized["created"] = now
		}
		if owner, _ := normalized["owned_by"].(string); owner == "" {
			normalized["owned_by"] = "xai"
		}
		models = append(models, normalized)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func fallbackModels(defaultModel string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": defaultModel, "object": "model", "created": now, "owned_by": "xai"},
			{"id": "grok-composer-2.5-fast", "object": "model", "created": now, "owned_by": "xai"},
		},
	}
}

func (s *Server) billing(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid API key")
		return
	}
	result, err := s.gateway.Request(request.Context(), http.MethodGet, "/billing", nil, false)
	if err != nil {
		s.writeGatewayError(writer, err)
		return
	}
	s.writeResult(writer, result)
}

func (s *Server) responses(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid API key")
		return
	}
	body, stream, ok := s.normalizedJSONBody(writer, request)
	if !ok {
		return
	}
	result, err := s.gateway.Request(
		request.Context(),
		http.MethodPost,
		"/responses",
		body,
		stream,
	)
	if err != nil {
		s.writeGatewayError(writer, err)
		return
	}
	s.writeResult(writer, result)
}

func (s *Server) messages(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid API key")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 32<<20))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request body")
		return
	}
	openAIRequest, stream, err := compat.AnthropicToOpenAI(body, s.defaultModel)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid Anthropic request")
		return
	}
	result, err := s.gateway.Chat(request.Context(), openAIRequest, stream)
	if err != nil {
		s.writeGatewayError(writer, err)
		return
	}
	if result.Status >= http.StatusBadRequest {
		s.writeResult(writer, result)
		return
	}
	if stream {
		if result.Stream == nil {
			writeOpenAIError(writer, http.StatusBadGateway, "Upstream stream missing")
			return
		}
		result.Stream = compat.NewAnthropicStream(
			result.Stream,
			requestModel(openAIRequest, s.defaultModel),
		)
		if result.Header == nil {
			result.Header = make(http.Header)
		}
		result.Header.Del("Content-Length")
		result.Header.Set("Content-Type", "text/event-stream")
		s.writeResult(writer, result)
		return
	}
	converted, err := compat.OpenAIToAnthropic(result.Body)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadGateway, "Invalid upstream response")
		return
	}
	result.Body = converted
	if result.Header == nil {
		result.Header = make(http.Header)
	}
	result.Header.Del("Content-Length")
	result.Header.Set("Content-Type", "application/json")
	s.writeResult(writer, result)
}

func (s *Server) normalizedJSONBody(
	writer http.ResponseWriter,
	request *http.Request,
) ([]byte, bool, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 32<<20))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request body")
		return nil, false, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid JSON body")
		return nil, false, false
	}
	if model, ok := payload["model"].(string); !ok || strings.TrimSpace(model) == "" {
		payload["model"] = s.defaultModel
	}
	stream, _ := payload["stream"].(bool)
	body, err = json.Marshal(payload)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request payload")
		return nil, false, false
	}
	return body, stream, true
}

func requestModel(payload []byte, fallback string) string {
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &request); err != nil || request.Model == "" {
		return fallback
	}
	return request.Model
}

func (s *Server) writeGatewayError(writer http.ResponseWriter, err error) {
	if poolError, ok := service.AsPoolUnavailable(err); ok {
		writer.Header().Set(
			"Retry-After",
			strconv.FormatInt(max(1, int64(poolError.RetryAfter/time.Second)), 10),
		)
		writeOpenAIError(writer, poolError.Status, "No ready accounts; retry later")
		return
	}
	writeOpenAIError(writer, http.StatusBadGateway, err.Error())
}

func (s *Server) writeResult(writer http.ResponseWriter, result service.ChatResult) {
	for name, values := range result.Header {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(result.Status)
	if result.Stream != nil {
		defer result.Stream.Close()
		buffer := make([]byte, 32*1024)
		for {
			count, readErr := result.Stream.Read(buffer)
			if count > 0 {
				if _, writeErr := writer.Write(buffer[:count]); writeErr != nil {
					return
				}
				if flusher, ok := writer.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if readErr != nil {
				return
			}
		}
	}
	_, _ = writer.Write(result.Body)
}

func (s *Server) authorized(request *http.Request) bool {
	return authorizedWithKey(request, s.apiKey)
}

func authorizedWithKey(request *http.Request, key string) bool {
	if key == "" {
		return true
	}
	token := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(token) >= len(prefix) && token[:len(prefix)] == prefix {
		token = token[len(prefix):]
	} else {
		token = request.Header.Get("x-api-key")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1
}

func writeOpenAIError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "api_error",
			"code":    strconv.Itoa(status),
			"param":   nil,
		},
	})
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
