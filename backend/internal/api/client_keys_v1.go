package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/clientkeys"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type ClientKeyLifecycle interface {
	Create(context.Context, clientkeys.CreateRequest) (clientkeys.Result, error)
	Get(context.Context, string) (clientkeys.Result, error)
	List(context.Context, repository.ListClientKeysQuery) (clientkeys.ListResult, error)
	Update(context.Context, string, clientkeys.UpdateRequest) (clientkeys.Result, error)
	Revoke(context.Context, string) (clientkeys.Result, error)
}

type ClientKeyAdminAuthorizer func(*http.Request) bool

type clientKeyAdminAPI struct {
	service   ClientKeyLifecycle
	authorize ClientKeyAdminAuthorizer
}

// NewClientKeyAdminHandler returns a standalone handler for the versioned
// client-key administration surface. Integration can mount the same routes on
// the main mux with RegisterClientKeyAdminRoutes.
func NewClientKeyAdminHandler(service ClientKeyLifecycle, authorize ClientKeyAdminAuthorizer) http.Handler {
	mux := http.NewServeMux()
	RegisterClientKeyAdminRoutes(mux, service, authorize)
	return mux
}

func RegisterClientKeyAdminRoutes(
	mux *http.ServeMux,
	service ClientKeyLifecycle,
	authorize ClientKeyAdminAuthorizer,
) {
	if mux == nil || service == nil {
		return
	}
	api := &clientKeyAdminAPI{service: service, authorize: authorize}
	mux.HandleFunc("GET /api/admin/v1/client-keys", api.list)
	mux.HandleFunc("POST /api/admin/v1/client-keys", api.create)
	mux.HandleFunc("GET /api/admin/v1/client-keys/{id}", api.get)
	mux.HandleFunc("PATCH /api/admin/v1/client-keys/{id}", api.update)
	mux.HandleFunc("POST /api/admin/v1/client-keys/{id}/revoke", api.revoke)
}

func (a *clientKeyAdminAPI) allowed(writer http.ResponseWriter, request *http.Request) bool {
	if a.authorize != nil && a.authorize(request) {
		return true
	}
	writeAdminError(writer, http.StatusUnauthorized, "unauthorized", "Administrator authentication is required")
	return false
}

func (a *clientKeyAdminAPI) list(writer http.ResponseWriter, request *http.Request) {
	if !a.allowed(writer, request) {
		return
	}
	values := request.URL.Query()
	page, _ := strconv.Atoi(strings.TrimSpace(values.Get("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(values.Get("page_size")))
	result, err := a.service.List(request.Context(), repository.ListClientKeysQuery{
		Q: strings.TrimSpace(values.Get("q")), Origin: clientkey.Origin(strings.TrimSpace(values.Get("origin"))),
		Page: page, PageSize: pageSize,
	})
	if err != nil {
		writeClientKeyAdminError(writer, err)
		return
	}
	items := make([]clientKeyDTO, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, clientKeyResultDTO(item, false))
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"items": items, "total": result.Total, "page": result.Page, "page_size": result.PageSize,
	})
}

func (a *clientKeyAdminAPI) create(writer http.ResponseWriter, request *http.Request) {
	if !a.allowed(writer, request) {
		return
	}
	var body struct {
		Name          string                 `json:"name"`
		ModelPolicy   *clientkey.ModelPolicy `json:"model_policy"`
		ModelScopes   []string               `json:"model_scopes"`
		RPMLimit      *int                   `json:"rpm_limit"`
		MaxConcurrent *int                   `json:"max_concurrent"`
		ExpiresAt     time.Time              `json:"expires_at"`
	}
	if err := decodeStrictJSON(writer, request, &body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.ModelPolicy == nil || body.RPMLimit == nil || body.MaxConcurrent == nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_request", "model_policy, rpm_limit, and max_concurrent are required")
		return
	}
	result, err := a.service.Create(request.Context(), clientkeys.CreateRequest{
		Name: body.Name, ModelPolicy: *body.ModelPolicy, Scopes: body.ModelScopes,
		RPMLimit: *body.RPMLimit, MaxConcurrent: *body.MaxConcurrent, ExpiresAt: body.ExpiresAt,
	})
	if err != nil {
		writeClientKeyAdminError(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/admin/v1/client-keys/"+url.PathEscape(result.Key.ID))
	writer.Header().Set("Cache-Control", "no-store")
	writeAdminOK(writer, http.StatusCreated, clientKeyResultDTO(result, true))
}

func (a *clientKeyAdminAPI) get(writer http.ResponseWriter, request *http.Request) {
	if !a.allowed(writer, request) {
		return
	}
	result, err := a.service.Get(request.Context(), request.PathValue("id"))
	if err != nil {
		writeClientKeyAdminError(writer, err)
		return
	}
	writeAdminOK(writer, http.StatusOK, clientKeyResultDTO(result, false))
}

func (a *clientKeyAdminAPI) update(writer http.ResponseWriter, request *http.Request) {
	if !a.allowed(writer, request) {
		return
	}
	var body struct {
		Name          *string                `json:"name"`
		ModelPolicy   *clientkey.ModelPolicy `json:"model_policy"`
		ModelScopes   *[]string              `json:"model_scopes"`
		RPMLimit      *int                   `json:"rpm_limit"`
		MaxConcurrent *int                   `json:"max_concurrent"`
		ExpiresAt     nullableTime           `json:"expires_at"`
	}
	if err := decodeStrictJSON(writer, request, &body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	id := request.PathValue("id")
	current, err := a.service.Get(request.Context(), id)
	if err != nil {
		writeClientKeyAdminError(writer, err)
		return
	}
	update := clientkeys.UpdateRequest{
		Name: current.Key.Name, ModelPolicy: current.Key.ModelPolicy, Scopes: current.Scopes,
		RPMLimit: current.Key.RPMLimit, MaxConcurrent: current.Key.MaxConcurrent, ExpiresAt: current.Key.ExpiresAt,
	}
	if body.Name != nil {
		update.Name = *body.Name
	}
	if body.ModelPolicy != nil {
		update.ModelPolicy = *body.ModelPolicy
		if update.ModelPolicy == clientkey.ModelPolicyAll && body.ModelScopes == nil {
			update.Scopes = nil
		}
	}
	if body.ModelScopes != nil {
		update.Scopes = *body.ModelScopes
	}
	if body.RPMLimit != nil {
		update.RPMLimit = *body.RPMLimit
	}
	if body.MaxConcurrent != nil {
		update.MaxConcurrent = *body.MaxConcurrent
	}
	if body.ExpiresAt.Set {
		update.ExpiresAt = body.ExpiresAt.Value
	}
	result, err := a.service.Update(request.Context(), id, update)
	if err != nil {
		writeClientKeyAdminError(writer, err)
		return
	}
	writeAdminOK(writer, http.StatusOK, clientKeyResultDTO(result, false))
}

func (a *clientKeyAdminAPI) revoke(writer http.ResponseWriter, request *http.Request) {
	if !a.allowed(writer, request) {
		return
	}
	result, err := a.service.Revoke(request.Context(), request.PathValue("id"))
	if err != nil {
		writeClientKeyAdminError(writer, err)
		return
	}
	writeAdminOK(writer, http.StatusOK, clientKeyResultDTO(result, false))
}

type nullableTime struct {
	Set   bool
	Value time.Time
}

func (value *nullableTime) UnmarshalJSON(payload []byte) error {
	value.Set = true
	if strings.TrimSpace(string(payload)) == "null" {
		value.Value = time.Time{}
		return nil
	}
	var parsed time.Time
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return err
	}
	value.Value = parsed
	return nil
}

type clientKeyDTO struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	Origin        clientkey.Origin      `json:"origin"`
	KeyPrefix     string                `json:"key_prefix"`
	ModelPolicy   clientkey.ModelPolicy `json:"model_policy"`
	ModelScopes   []string              `json:"model_scopes"`
	RPMLimit      int                   `json:"rpm_limit"`
	MaxConcurrent int                   `json:"max_concurrent"`
	ExpiresAt     *time.Time            `json:"expires_at"`
	RevokedAt     *time.Time            `json:"revoked_at"`
	LastUsedAt    *time.Time            `json:"last_used_at"`
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
	Secret        string                `json:"secret,omitempty"`
}

func clientKeyResultDTO(result clientkeys.Result, includeSecret bool) clientKeyDTO {
	item := clientKeyDTO{
		ID: result.Key.ID, Name: result.Key.Name, Origin: result.Key.Origin, KeyPrefix: result.Key.KeyPrefix,
		ModelPolicy: result.Key.ModelPolicy, ModelScopes: append([]string(nil), result.Scopes...),
		RPMLimit: result.Key.RPMLimit, MaxConcurrent: result.Key.MaxConcurrent,
		CreatedAt: result.Key.CreatedAt, UpdatedAt: result.Key.UpdatedAt,
	}
	item.ExpiresAt = timePointer(result.Key.ExpiresAt)
	item.RevokedAt = timePointer(result.Key.RevokedAt)
	item.LastUsedAt = timePointer(result.Key.LastUsedAt)
	if includeSecret {
		item.Secret = result.Secret
	}
	return item
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func decodeStrictJSON(writer http.ResponseWriter, request *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeClientKeyAdminError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, clientkeys.ErrNotFound):
		writeAdminError(writer, http.StatusNotFound, "client_key_not_found", "Client key not found")
	case errors.Is(err, clientkeys.ErrRevoked):
		writeAdminError(writer, http.StatusConflict, "client_key_revoked", "Client key is revoked and cannot be restored")
	case errors.Is(err, clientkeys.ErrExpired):
		writeAdminError(writer, http.StatusConflict, "client_key_expired", "Client key is expired and cannot be restored")
	case errors.Is(err, clientkeys.ErrInvalid):
		writeAdminError(writer, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeAdminError(writer, http.StatusInternalServerError, "internal_error", "Client key operation failed")
	}
}
