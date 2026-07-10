package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
	"github.com/AokiAx/grok2api/internal/service"
)

//go:embed panel.html
var panelHTML []byte

type ChatGateway interface {
	Chat(context.Context, []byte, bool) (service.ChatResult, error)
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
	gateway      ChatGateway
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

func NewServer(gateway ChatGateway, status StatusProvider, apiKey string, options ...Option) *Server {
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
	mux.HandleFunc("POST /v1/chat/completions", server.chat)
	mux.HandleFunc("POST /chat/completions", server.chat)
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
		if poolError, ok := service.AsPoolUnavailable(err); ok {
			writer.Header().Set(
				"Retry-After",
				strconv.FormatInt(max(1, int64(poolError.RetryAfter/time.Second)), 10),
			)
			writeOpenAIError(writer, poolError.Status, "No ready accounts; retry later")
			return
		}
		writeOpenAIError(writer, http.StatusBadGateway, err.Error())
		return
	}
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
