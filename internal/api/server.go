package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
	"github.com/AokiAx/grok2api/internal/compat"
	"github.com/AokiAx/grok2api/internal/config"
	"github.com/AokiAx/grok2api/internal/register"
	regsettings "github.com/AokiAx/grok2api/internal/register/settings"
	"github.com/AokiAx/grok2api/internal/service"
	"github.com/AokiAx/grok2api/internal/upstream"
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
	gateway          Gateway
	status           StatusProvider
	apiKey           string
	defaultModel     string
	modelCatalog     *upstream.Catalog
	preferResponses  bool
	admin            AdminService
	adminKey         string
	registerJobs     RegisterJobService
	registerSettings RegisterSettingsService
	handler          http.Handler
}

type Option func(*Server)

func WithDefaultModel(model string) Option {
	return func(server *Server) {
		server.defaultModel = model
	}
}

func WithModelCatalog(catalog *upstream.Catalog) Option {
	return func(server *Server) {
		if catalog != nil {
			server.modelCatalog = catalog
		}
	}
}

func WithPreferResponses(enabled bool) Option {
	return func(server *Server) {
		server.preferResponses = enabled
	}
}

type AdminService interface {
	List(context.Context) ([]account.Account, error)
	Import(context.Context, admin.ImportRequest) (admin.ImportResult, error)
	Delete(context.Context, string) error
	Recover(context.Context, string) (account.Account, error)
}

func WithAdmin(service AdminService, key string) Option {
	return func(server *Server) {
		server.admin = service
		server.adminKey = key
	}
}

type RegisterJobService interface {
	Start(register.RunConfig) (string, error)
	Stop() error
	Status() register.JobStatus
	Health(context.Context) register.HealthReport
	Settings() config.Config
}

type RegisterSettingsService interface {
	Get() config.Config
	Update(config.Config) (config.Config, error)
}

func WithRegisterJobs(service RegisterJobService) Option {
	return func(server *Server) {
		server.registerJobs = service
	}
}

func WithRegisterSettings(service RegisterSettingsService) Option {
	return func(server *Server) {
		server.registerSettings = service
	}
}

func NewServer(gateway Gateway, status StatusProvider, apiKey string, options ...Option) *Server {
	server := &Server{
		gateway:         gateway,
		status:          status,
		apiKey:          apiKey,
		defaultModel:    "grok-4.5",
		modelCatalog:    upstream.NewDefaultCatalog(),
		preferResponses: true,
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
		mux.HandleFunc("DELETE /admin/api/cli-accounts/{id}", server.adminDelete)
		mux.HandleFunc("POST /admin/api/cli-accounts/{id}/recover", server.adminRecover)
		mux.HandleFunc("POST /admin/api/accounts/import/preview", server.adminImportPreview)
		mux.HandleFunc("POST /admin/api/accounts/import", server.adminImport)
	}
	if server.registerJobs != nil {
		mux.HandleFunc("GET /admin/api/register/status", server.registerStatus)
		mux.HandleFunc("POST /admin/api/register/start", server.registerStart)
		mux.HandleFunc("POST /admin/api/register/stop", server.registerStop)
		mux.HandleFunc("GET /admin/api/register/health", server.registerHealth)
	}
	if server.registerSettings != nil {
		mux.HandleFunc("GET /admin/api/register/settings", server.registerSettingsGet)
		mux.HandleFunc("PUT /admin/api/register/settings", server.registerSettingsPut)
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
		public = append(public, publicAccount(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"count":    len(accounts),
		"accounts": public,
		"summary":  summarizeAccounts(accounts),
	})
}

type accountSummary struct {
	TotalAccounts       int            `json:"total_accounts"`
	ReadyAccounts       int            `json:"ready_accounts"`
	UnavailableAccounts int            `json:"unavailable_accounts"`
	ActiveLeases        int            `json:"active_leases"`
	MaxActive           int            `json:"max_active"`
	TotalRequests       int64          `json:"total_requests"`
	RefreshableAccounts int            `json:"refreshable_accounts"`
	QuotaActual         int64          `json:"quota_actual"`
	QuotaLimit          int64          `json:"quota_limit"`
	Reasons             map[string]int `json:"reasons"`
}

func summarizeAccounts(accounts []account.Account) accountSummary {
	summary := accountSummary{
		TotalAccounts: len(accounts),
		Reasons:       make(map[string]int),
	}
	for _, item := range accounts {
		if item.Pool == account.PoolReady {
			summary.ReadyAccounts++
		} else {
			summary.UnavailableAccounts++
			summary.Reasons[string(item.UnavailableReason)]++
		}
		summary.ActiveLeases += item.Active
		maxActive := item.MaxActive
		if maxActive <= 0 {
			maxActive = 1
		}
		summary.MaxActive += maxActive
		summary.TotalRequests += item.RequestCount
		if item.RefreshToken != "" {
			summary.RefreshableAccounts++
		}
		summary.QuotaActual += item.QuotaActual
		summary.QuotaLimit += item.QuotaLimit
	}
	return summary
}

func publicAccount(item account.Account) map[string]any {
	return map[string]any{
		"id":                 item.ID,
		"email":              item.Email,
		"user_id":            item.UserID,
		"team_id":            item.TeamID,
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
	}
}

func (s *Server) adminDelete(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	id := request.PathValue("id")
	if err := s.admin.Delete(request.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, admin.ErrAccountNotFound) {
			status = http.StatusNotFound
		}
		writeOpenAIError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

func (s *Server) adminRecover(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	item, err := s.admin.Recover(request.Context(), request.PathValue("id"))
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, admin.ErrAccountNotFound) {
			status = http.StatusNotFound
		}
		writeOpenAIError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, publicAccount(item))
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

func (s *Server) registerStatus(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	if s.registerJobs == nil {
		writeOpenAIError(writer, http.StatusNotFound, "register jobs unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, s.registerJobs.Status())
}

func (s *Server) registerHealth(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	if s.registerJobs == nil {
		writeOpenAIError(writer, http.StatusNotFound, "register jobs unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, s.registerJobs.Health(request.Context()))
}

func (s *Server) registerStart(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	if s.registerJobs == nil {
		writeOpenAIError(writer, http.StatusNotFound, "register jobs unavailable")
		return
	}
	var payload struct {
		Count   int    `json:"count"`
		Workers int    `json:"workers"`
		DryRun  bool   `json:"dry_run"`
		Proxy   string `json:"proxy"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&payload)
	jobID, err := s.registerJobs.Start(register.RunConfig{
		Count:    payload.Count,
		Workers:  payload.Workers,
		DryRun:   payload.DryRun,
		ProxyURL: payload.Proxy,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, register.ErrJobRunning) {
			status = http.StatusConflict
		}
		writeOpenAIError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"job_id": jobID, "started": true})
}

func (s *Server) registerStop(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	if s.registerJobs == nil {
		writeOpenAIError(writer, http.StatusNotFound, "register jobs unavailable")
		return
	}
	if err := s.registerJobs.Stop(); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, register.ErrNoJob) {
			status = http.StatusConflict
		}
		writeOpenAIError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"stopped": true})
}

func (s *Server) registerSettingsGet(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	if s.registerSettings == nil {
		writeOpenAIError(writer, http.StatusNotFound, "register settings unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, regsettings.EditorView(s.registerSettings.Get()))
}

func (s *Server) registerSettingsPut(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid admin key")
		return
	}
	if s.registerSettings == nil {
		writeOpenAIError(writer, http.StatusNotFound, "register settings unavailable")
		return
	}
	var patch config.Config
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 2<<20)).Decode(&patch); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid settings payload")
		return
	}
	updated, err := s.registerSettings.Update(patch)
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"saved":    true,
		"settings": regsettings.EditorView(updated),
		"summary":  regsettings.PublicView(updated),
	})
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	status := s.status.PoolStatus()
	payload := map[string]any{
		"ok":           status.Ready > 0,
		"version":      "1.0.0-go",
		"account_pool": status,
	}
	if provider, ok := s.gateway.(interface {
		CircuitStatus() service.CircuitStatus
	}); ok {
		payload["quota_circuit"] = provider.CircuitStatus()
	}
	writeJSON(writer, http.StatusOK, payload)
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
	body, model, clientStream, err := compat.NormalizeChatRequest(body, s.defaultModel)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	backend := upstream.BackendChatCompletions
	if s.preferResponses {
		backend = s.modelCatalog.Backend(model)
	}

	var result service.ChatResult
	if backend == upstream.BackendResponses {
		responsesBody, _, convertErr := compat.ChatToResponses(body)
		if convertErr != nil {
			writeOpenAIError(writer, http.StatusBadRequest, "Invalid chat payload")
			return
		}
		// Upstream prefers streaming for Responses-backed models.
		result, err = s.gateway.Request(
			request.Context(),
			http.MethodPost,
			"/responses",
			responsesBody,
			true,
		)
		if err != nil {
			s.writeGatewayError(writer, err)
			return
		}
		if result.Status >= http.StatusBadRequest {
			if result.Stream != nil {
				data, readErr := io.ReadAll(result.Stream)
				_ = result.Stream.Close()
				if readErr == nil {
					result.Body = data
					result.Stream = nil
				}
			}
			s.writeResult(writer, result)
			return
		}
		if clientStream {
			if result.Stream == nil {
				writeOpenAIError(writer, http.StatusBadGateway, "Upstream stream missing")
				return
			}
			result.Stream = compat.NewResponsesToChatStream(result.Stream, model)
			if result.Header == nil {
				result.Header = make(http.Header)
			}
			result.Header.Del("Content-Length")
			result.Header.Set("Content-Type", "text/event-stream")
			s.writeResult(writer, result)
			return
		}
		if result.Stream != nil {
			aggregated, aggErr := compat.AggregateResponsesStream(result.Stream, model)
			if aggErr != nil {
				writeOpenAIError(writer, http.StatusBadGateway, "Invalid upstream stream")
				return
			}
			result.Body = aggregated
			result.Stream = nil
			if result.Header == nil {
				result.Header = make(http.Header)
			}
			result.Header.Del("Content-Length")
			result.Header.Set("Content-Type", "application/json")
			s.writeResult(writer, result)
			return
		}
		converted, convErr := compat.ResponsesToChat(result.Body)
		if convErr != nil {
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
		return
	}

	result, err = s.gateway.Chat(request.Context(), body, clientStream)
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
		normalized := make(map[string]any, len(item)+8)
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
		if s.modelCatalog != nil {
			normalized = s.modelCatalog.EnrichModelMap(normalized)
		}
		models = append(models, normalized)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func fallbackModels(defaultModel string) map[string]any {
	now := time.Now().Unix()
	catalog := upstream.NewDefaultCatalog()
	data := make([]map[string]any, 0, 4)
	seen := map[string]struct{}{}
	for _, item := range catalog.List() {
		entry := map[string]any{
			"id":          item.ID,
			"object":      "model",
			"created":     now,
			"owned_by":    firstNonEmpty(item.OwnedBy, "xai"),
			"name":        item.Name,
			"api_backend": item.APIBackend,
		}
		if item.ContextWindow > 0 {
			entry["context_window"] = item.ContextWindow
		}
		if item.SupportsReasoningEffort {
			entry["supports_reasoning_effort"] = true
			entry["reasoning_efforts"] = item.ReasoningEfforts
		}
		if item.SupportsBackendSearch {
			entry["supports_backend_search"] = true
		}
		data = append(data, entry)
		seen[item.ID] = struct{}{}
	}
	if _, ok := seen[defaultModel]; !ok && defaultModel != "" {
		data = append([]map[string]any{{
			"id":          defaultModel,
			"object":      "model",
			"created":     now,
			"owned_by":    "xai",
			"api_backend": catalog.Backend(defaultModel),
		}}, data...)
	}
	return map[string]any{"object": "list", "data": data}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
	if normalized, _, _, err := compat.NormalizeChatRequest(body, s.defaultModel); err == nil {
		var payload map[string]any
		if json.Unmarshal(normalized, &payload) == nil {
			if _, hasInput := payload["input"]; !hasInput {
				if messages, ok := payload["messages"]; ok {
					payload["input"] = messages
					delete(payload, "messages")
				}
			}
			if maxTokens, ok := payload["max_tokens"]; ok {
				if _, exists := payload["max_output_tokens"]; !exists {
					payload["max_output_tokens"] = maxTokens
				}
				delete(payload, "max_tokens")
			}
			if encoded, err := json.Marshal(payload); err == nil {
				body = encoded
			}
		}
	}
	if !stream {
		model := requestModel(body, s.defaultModel)
		if s.modelCatalog != nil && s.modelCatalog.Backend(model) == upstream.BackendResponses {
			result, err := s.gateway.Request(request.Context(), http.MethodPost, "/responses", body, true)
			if err != nil {
				s.writeGatewayError(writer, err)
				return
			}
			if result.Status >= 400 {
				s.writeResult(writer, result)
				return
			}
			if result.Stream != nil {
				data, err := io.ReadAll(result.Stream)
				_ = result.Stream.Close()
				if err != nil {
					writeOpenAIError(writer, http.StatusBadGateway, "Invalid upstream stream")
					return
				}
				if completed := extractCompletedResponse(data); len(completed) > 0 {
					result.Body = completed
				} else {
					result.Body = data
				}
				result.Stream = nil
				if result.Header == nil {
					result.Header = make(http.Header)
				}
				result.Header.Del("Content-Length")
				result.Header.Set("Content-Type", "application/json")
			}
			s.writeResult(writer, result)
			return
		}
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

func extractCompletedResponse(sseOrJSON []byte) []byte {
	text := string(sseOrJSON)
	if !strings.Contains(text, "event:") && json.Valid(sseOrJSON) {
		return sseOrJSON
	}
	var last []byte
	for _, block := range strings.Split(text, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var payload map[string]any
			if json.Unmarshal([]byte(data), &payload) != nil {
				continue
			}
			if payload["type"] == "response.completed" {
				if response, ok := payload["response"].(map[string]any); ok {
					if encoded, err := json.Marshal(response); err == nil {
						last = encoded
					}
				}
			}
		}
	}
	return last
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
	// Prefer the same responses backend as OpenAI chat for catalog models so
	// free-quota headers / prompt-cache behavior stay consistent.
	model := requestModel(openAIRequest, s.defaultModel)
	var result service.ChatResult
	if s.preferResponses && s.modelCatalog != nil && s.modelCatalog.Backend(model) == upstream.BackendResponses {
		responsesBody, _, convertErr := compat.ChatToResponses(openAIRequest)
		if convertErr != nil {
			writeOpenAIError(writer, http.StatusBadRequest, "Invalid Anthropic request")
			return
		}
		result, err = s.gateway.Request(request.Context(), http.MethodPost, "/responses", responsesBody, true)
		if err != nil {
			s.writeGatewayError(writer, err)
			return
		}
		if result.Status >= http.StatusBadRequest {
			if result.Stream != nil {
				data, readErr := io.ReadAll(result.Stream)
				_ = result.Stream.Close()
				if readErr == nil {
					result.Body = data
					result.Stream = nil
				}
			}
			s.writeResult(writer, result)
			return
		}
		if stream {
			if result.Stream == nil {
				writeOpenAIError(writer, http.StatusBadGateway, "Upstream stream missing")
				return
			}
			// responses SSE -> chat SSE -> anthropic SSE
			chatStream := compat.NewResponsesToChatStream(result.Stream, model)
			result.Stream = compat.NewAnthropicStream(chatStream, model)
			if result.Header == nil {
				result.Header = make(http.Header)
			}
			result.Header.Del("Content-Length")
			result.Header.Set("Content-Type", "text/event-stream")
			s.writeResult(writer, result)
			return
		}
		if result.Stream != nil {
			aggregated, aggErr := compat.AggregateResponsesStream(result.Stream, model)
			if aggErr != nil {
				writeOpenAIError(writer, http.StatusBadGateway, "Invalid upstream stream")
				return
			}
			result.Body = aggregated
			result.Stream = nil
		} else {
			convertedChat, convErr := compat.ResponsesToChat(result.Body)
			if convErr != nil {
				writeOpenAIError(writer, http.StatusBadGateway, "Invalid upstream response")
				return
			}
			result.Body = convertedChat
		}
		converted, convErr := compat.OpenAIToAnthropic(result.Body)
		if convErr != nil {
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
		return
	}

	result, err = s.gateway.Chat(request.Context(), openAIRequest, stream)
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
