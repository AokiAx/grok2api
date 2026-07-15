package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/admin"
	authservice "github.com/AokiAx/grok2api/backend/internal/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/bridge"
	"github.com/AokiAx/grok2api/backend/internal/clientkeys"
	"github.com/AokiAx/grok2api/backend/internal/compat"
	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/intercept"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/requestctx"
	"github.com/AokiAx/grok2api/backend/internal/service"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

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

// LiveLeaseProvider exposes in-memory lease counts for admin views.
// Optional: when absent, active concurrency falls back to zero from SQLite.
type LiveLeaseProvider interface {
	ActiveByID() map[string]int
}

type Server struct {
	gateway          Gateway
	status           StatusProvider
	apiKey           string
	defaultModel     string
	modelCatalog     *upstream.Catalog
	preferResponses  bool
	bridge           *bridge.Pipeline
	tracer           *intercept.Tracer
	admin            AdminService
	adminKey         string
	adminAuth        *authservice.Service
	adminAuthOptions AdminAuthHandlerOptions
	clientAccess     *service.ClientAccess
	clientKeys       ClientKeyLifecycle
	frontend         fs.FS
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

// WithDebugTrace enables the temporary request interceptor (JSONL + slog).
func WithDebugTrace(tracer *intercept.Tracer) Option {
	return func(server *Server) {
		server.tracer = tracer
	}
}

type AdminService interface {
	ListPage(context.Context, admin.ListAccountsQuery) (admin.ListAccountsPage, error)
	Stats(context.Context) (admin.AccountStats, error)
	Import(context.Context, admin.ImportRequest) (admin.ImportResult, error)
	Delete(context.Context, string) error
	Recover(context.Context, string) (account.Account, error)
	Get(context.Context, string) (account.Account, error)
	Update(context.Context, string, admin.UpdateAccountRequest) (account.Account, error)
	Batch(context.Context, admin.BatchAccountRequest) (admin.BatchAccountResult, error)
	Events(context.Context, string, int, int) (repository.ListAccountEventsResult, error)
	RefreshCredential(context.Context, string) (account.Account, error)
	RefreshQuota(context.Context, string) (admin.QuotaRefreshResult, error)
	ExportCredential(context.Context, string) (admin.CredentialExport, error)
}

func WithAdmin(service AdminService, key string) Option {
	return func(server *Server) {
		server.admin = service
		server.adminKey = key
	}
}

func WithAdminAuth(auth *authservice.Service, options AdminAuthHandlerOptions) Option {
	return func(server *Server) {
		server.adminAuth = auth
		server.adminAuthOptions = options
	}
}

func WithClientAccess(access *service.ClientAccess) Option {
	return func(server *Server) {
		server.clientAccess = access
	}
}

func WithClientKeys(keys *clientkeys.Service) Option {
	return func(server *Server) {
		server.clientKeys = keys
	}
}

// WithFrontend mounts a pre-validated SPA filesystem at the service root.
// The filesystem root must contain index.html and any referenced assets.
func WithFrontend(frontend fs.FS) Option {
	return func(server *Server) {
		server.frontend = frontend
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
	server.bridge = &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         server.modelCatalog,
		DefaultModel:    server.defaultModel,
		PreferResponses: server.preferResponses,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.health)
	modelsHandler := http.Handler(http.HandlerFunc(server.models))
	billingHandler := http.Handler(http.HandlerFunc(server.billing))
	chatHandler := http.Handler(http.HandlerFunc(server.chat))
	responsesHandler := http.Handler(http.HandlerFunc(server.responses))
	messagesHandler := http.Handler(http.HandlerFunc(server.messages))
	if server.clientAccess != nil {
		modelsHandler = ClientModelsMiddleware(server.clientAccess, modelsHandler)
		billingHandler = ClientAuthMiddleware(server.clientAccess, billingHandler)
		chatHandler = ClientInferenceMiddleware(server.clientAccess, server.defaultModel, chatHandler)
		responsesHandler = ClientInferenceMiddleware(server.clientAccess, server.defaultModel, responsesHandler)
		messagesHandler = ClientInferenceMiddleware(server.clientAccess, server.defaultModel, messagesHandler)
	}
	mux.Handle("GET /v1/models", modelsHandler)
	mux.Handle("GET /v1/billing", billingHandler)
	mux.Handle("POST /v1/chat/completions", chatHandler)
	mux.Handle("POST /chat/completions", chatHandler)
	mux.Handle("POST /v1/responses", responsesHandler)
	mux.Handle("POST /v1/messages", messagesHandler)
	if server.frontend != nil {
		server.registerSPARoutes(mux)
	}
	server.registerAdminRoutes(mux)
	var handler http.Handler = mux
	if server.tracer != nil && server.tracer.Enabled() {
		// Temporary protocol debugger: client ↔ bridge ↔ upstream stages.
		handler = intercept.Middleware(server.tracer, mux)
	}
	server.handler = handler
	return server
}

func (s *Server) panelMeta(writer http.ResponseWriter, request *http.Request) {
	setupRequired, _ := s.adminSetupRequired(request.Context())
	writeJSON(writer, http.StatusOK, map[string]any{
		"auth_required":  s.adminAuth != nil || s.adminKey != "",
		"setup_required": setupRequired,
		"version":        "1.0.0-go",
	})
}

func (s *Server) adminList(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	payload, err := s.buildAdminListPayload(request)
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, payload)
}

func parseAdminListQuery(request *http.Request) admin.ListAccountsQuery {
	values := request.URL.Query()
	page, _ := strconv.Atoi(strings.TrimSpace(values.Get("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(values.Get("page_size")))
	pool := strings.TrimSpace(values.Get("pool"))
	if pool == "all" {
		pool = ""
	}
	return admin.ListAccountsQuery{
		Pool:     pool,
		Q:        strings.TrimSpace(values.Get("q")),
		Page:     page,
		PageSize: pageSize,
	}
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
		"priority":           item.Priority,
		"has_refresh_token":  item.RefreshToken != "",
	}
}

func (s *Server) adminDelete(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
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
	if !s.requireAdmin(writer, request) {
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
	if !s.requireAdmin(writer, request) {
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
	body, err := readInferenceRequestBody(writer, request)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request body")
		return
	}
	result, err := s.bridge.Chat(withStickyContext(request), body)
	if err != nil {
		s.writeBridgeError(writer, err)
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
	body, err := readInferenceRequestBody(writer, request)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request body")
		return
	}
	result, err := s.bridge.Responses(withStickyContext(request), body)
	if err != nil {
		s.writeBridgeError(writer, err)
		return
	}
	s.writeResult(writer, result)
}

func (s *Server) messages(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeOpenAIError(writer, http.StatusUnauthorized, "Invalid API key")
		return
	}
	body, err := readInferenceRequestBody(writer, request)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "Invalid request body")
		return
	}
	// Session sticky: Claude Code session header → prompt_cache_key + x-grok-conv-id.
	// Account-pool sticky still uses withStickyContext (requestctx).
	result, err := s.bridge.Messages(withStickyContext(request), body, compat.SessionIDFromRequest(request))
	if err != nil {
		s.writeBridgeError(writer, err)
		return
	}
	s.writeResult(writer, result)
}

func (s *Server) writeBridgeError(writer http.ResponseWriter, err error) {
	if bridgeErr, ok := bridge.AsError(err); ok {
		status := http.StatusBadGateway
		if bridgeErr.Class == bridge.ClassInvalidRequest {
			status = http.StatusBadRequest
		}
		writeOpenAIError(writer, status, bridgeErr.Message)
		return
	}
	s.writeGatewayError(writer, err)
}

func (s *Server) writeGatewayError(writer http.ResponseWriter, err error) {
	if poolError, ok := service.AsPoolUnavailable(err); ok {
		writer.Header().Set(
			"Retry-After",
			strconv.FormatInt(max(1, int64(poolError.RetryAfter/time.Second)), 10),
		)
		if reason := string(poolError.Reason); reason != "" {
			writer.Header().Set("X-Grok2API-Pool-Reason", reason)
		}
		message := poolError.Error()
		if message == "" {
			message = "No ready accounts; retry later"
		}
		writeOpenAIErrorCode(writer, poolError.Status, string(poolError.Reason), message)
		return
	}
	writeOpenAIError(writer, http.StatusBadGateway, err.Error())
}

func (s *Server) writeResult(writer http.ResponseWriter, result service.ChatResult) {
	copyCompatibleUpstreamHeaders(writer.Header(), result.Header)
	if result.Status >= http.StatusInternalServerError {
		if result.Stream != nil {
			_ = result.Stream.Close()
		}
		writeOpenAIError(writer, result.Status, publicServerErrorMessage(result.Status))
		return
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
	if s.clientAccess != nil {
		return true
	}
	return authorizedWithKey(request, s.apiKey)
}

// withStickyContext attaches a sticky pool key from client headers / API key
// so continuous agent sessions stay on a warm Grok account.
func withStickyContext(request *http.Request) context.Context {
	return requestctx.WithStickyKey(request.Context(), service.StickyKeyFromRequest(request))
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
	writeOpenAIErrorCode(writer, status, strconv.Itoa(status), message)
}

func writeOpenAIErrorCode(writer http.ResponseWriter, status int, code, message string) {
	if code == "" {
		code = strconv.Itoa(status)
	}
	if status >= http.StatusInternalServerError {
		slog.Error("api request failed", "status", status, "code", code, "error", message)
		message = publicServerErrorMessage(status)
	}
	writeJSON(writer, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "api_error",
			"code":    code,
			"param":   nil,
		},
	})
}

func publicServerErrorMessage(status int) string {
	switch status {
	case http.StatusBadGateway:
		return "Upstream request failed"
	case http.StatusServiceUnavailable:
		return "Service temporarily unavailable"
	default:
		return "Internal server error"
	}
}

func copyCompatibleUpstreamHeaders(destination, source http.Header) {
	for name, values := range source {
		if !compatibleUpstreamHeader(name) {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func compatibleUpstreamHeader(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "content-type", "cache-control", "retry-after", "x-request-id", "x-grok-request-id":
		return true
	default:
		return strings.HasPrefix(name, "x-ratelimit-")
	}
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
