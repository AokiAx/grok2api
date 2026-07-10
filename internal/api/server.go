package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/AokiAx/grok2api/internal/service"
)

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
	handler      http.Handler
}

type Option func(*Server)

func WithDefaultModel(model string) Option {
	return func(server *Server) {
		server.defaultModel = model
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
	server.handler = mux
	return server
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
	if s.apiKey == "" {
		return true
	}
	token := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(token) >= len(prefix) && token[:len(prefix)] == prefix {
		token = token[len(prefix):]
	} else {
		token = request.Header.Get("x-api-key")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) == 1
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
