package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	auth "github.com/AokiAx/grok2api/backend/internal/adminauth"
)

type AdminAuthHandlerOptions struct {
	SecureCookies bool
	Clock         func() time.Time
}

func NewAdminAuthHandler(service *auth.Service, opts AdminAuthHandlerOptions) http.Handler {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	mux := http.NewServeMux()
	h := &adminAuthHandler{service: service, opts: opts}
	mux.HandleFunc("POST /api/admin/v1/auth/login", h.login)
	mux.HandleFunc("POST /api/admin/v1/auth/refresh", h.refresh)
	mux.HandleFunc("POST /api/admin/v1/auth/logout", h.logout)
	mux.HandleFunc("GET /api/admin/v1/auth/me", h.me)
	return mux
}

type adminAuthHandler struct {
	service *auth.Service
	opts    AdminAuthHandlerOptions
}
type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

func (h *adminAuthHandler) login(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		h.error(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	var in authRequest
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		h.error(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if strings.TrimSpace(in.Username) == "" || in.Password == "" {
		h.error(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	out, err := h.service.Login(r.Context(), auth.LoginInput{Username: in.Username, Password: in.Password, SourceIP: requestIP(r), UserAgent: r.UserAgent(), Remember: in.Remember})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.setCookie(w, out.RefreshCookieValue, out.Remember, out.RefreshExpiresAt)
	h.ok(w, authDTO(out))
}
func (h *adminAuthHandler) refresh(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		h.error(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	c, err := r.Cookie("grok2api_admin_refresh")
	if err != nil {
		h.error(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	out, err := h.service.Refresh(r.Context(), c.Value, requestIP(r), r.UserAgent())
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.setCookie(w, out.RefreshCookieValue, out.Remember, out.RefreshExpiresAt)
	h.ok(w, authDTO(out))
}
func (h *adminAuthHandler) logout(w http.ResponseWriter, r *http.Request) {
	h.deleteCookie(w)
	if h.service == nil {
		h.error(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	var logoutErr error
	if p := strings.Fields(r.Header.Get("Authorization")); len(p) == 2 && strings.EqualFold(p[0], "Bearer") {
		logoutErr = errors.Join(logoutErr, h.service.LogoutAccess(r.Context(), p[1]))
	}
	if c, e := r.Cookie("grok2api_admin_refresh"); e == nil {
		logoutErr = errors.Join(logoutErr, h.service.Logout(r.Context(), c.Value))
	}
	if logoutErr != nil {
		h.writeErr(w, logoutErr)
		return
	}
	h.ok(w, map[string]bool{"logged_out": true})
}
func (h *adminAuthHandler) me(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		h.error(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		h.error(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	u, _, err := h.service.AuthenticateAccess(r.Context(), parts[1])
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.ok(w, map[string]any{"id": u.ID, "username": u.Username, "role": u.Role})
}
func (h *adminAuthHandler) deleteCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "grok2api_admin_refresh", Value: "", Path: "/api/admin/v1/auth", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: h.opts.SecureCookies})
}
func (h *adminAuthHandler) setCookie(w http.ResponseWriter, v string, remember bool, exp time.Time) {
	max := 0
	cookie := &http.Cookie{Name: "grok2api_admin_refresh", Value: v, Path: "/api/admin/v1/auth", MaxAge: max, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: h.opts.SecureCookies}
	if remember {
		max = int(exp.Sub(h.opts.Clock()).Seconds())
		if max < 1 {
			max = 1
		}
		cookie.MaxAge = max
		cookie.Expires = exp
	}
	http.SetCookie(w, cookie)
}
func (h *adminAuthHandler) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}
func authDTO(out auth.LoginOutput) map[string]any {
	return map[string]any{"admin": map[string]any{"id": out.Admin.ID, "username": out.Admin.Username, "role": out.Admin.Role}, "tokens": map[string]any{"accessToken": out.AccessToken, "accessTokenExpiresAt": out.AccessExpiresAt, "refreshTokenExpiresAt": out.RefreshExpiresAt}}
}
func (h *adminAuthHandler) error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
	}
	if status == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "900")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
}
func (h *adminAuthHandler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrSetupRequired):
		h.error(w, http.StatusServiceUnavailable, "setup_required", "")
	case errors.Is(err, auth.ErrInvalidCredentials):
		h.error(w, http.StatusUnauthorized, "invalid_credentials", "")
	case errors.Is(err, auth.ErrRateLimited):
		var limited *auth.LoginRateLimitError
		if errors.As(err, &limited) {
			seconds := int64((limited.RetryAfter + time.Second - 1) / time.Second)
			w.Header().Set("Retry-After", fmt.Sprint(seconds))
		}
		h.error(w, http.StatusTooManyRequests, "login_rate_limited", "")
	case errors.Is(err, auth.ErrConflict):
		h.error(w, http.StatusConflict, "refresh_conflict", "")
	case errors.Is(err, auth.ErrInvalidRefresh):
		h.error(w, http.StatusUnauthorized, "invalid_refresh_session", "")
	case errors.Is(err, auth.ErrUnauthorized):
		h.error(w, http.StatusUnauthorized, "unauthorized", "")
	default:
		h.error(w, http.StatusInternalServerError, "internal_error", "")
	}
}
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return strings.Trim(r.RemoteAddr, "[]")
}
