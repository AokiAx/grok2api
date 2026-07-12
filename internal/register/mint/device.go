package mint

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/admin"
)

const (
	Issuer   = "https://auth.x.ai"
	ClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	Scopes   = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
)

type TokenResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Email        string
	UserID       string
}

type DeviceMinter struct {
	client *http.Client
}

func NewDeviceMinter(client *http.Client) *DeviceMinter {
	if client == nil {
		jar, _ := cookiejar.New(nil)
		client = &http.Client{Timeout: 45 * time.Second, Jar: jar}
	}
	if client.Jar == nil {
		jar, _ := cookiejar.New(nil)
		clone := *client
		clone.Jar = jar
		client = &clone
	}
	return &DeviceMinter{client: client}
}

// NewDeviceMinterWithProxy builds a minter that uses the same proxy egress as
// the registration HTTP client (critical when accounts.x.ai is geo-restricted).
func NewDeviceMinterWithProxy(proxyURL string) (*DeviceMinter, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	var transport http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse mint proxy: %w", err)
		}
		transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
	}
	jar, _ := cookiejar.New(nil)
	return NewDeviceMinter(&http.Client{
		Timeout:   45 * time.Second,
		Jar:       jar,
		Transport: transport,
	}), nil
}

func (m *DeviceMinter) MintFromSSO(ctx context.Context, ssoCookie, email string) (TokenResult, error) {
	ssoCookie = strings.TrimSpace(ssoCookie)
	if ssoCookie == "" {
		return TokenResult{}, fmt.Errorf("sso cookie required")
	}
	accountsURL, _ := url.Parse("https://accounts.x.ai/")
	m.client.Jar.SetCookies(accountsURL, []*http.Cookie{{
		Name:   "sso",
		Value:  ssoCookie,
		Domain: ".x.ai",
		Path:   "/",
	}})
	warmReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://accounts.x.ai/", nil)
	if err != nil {
		return TokenResult{}, err
	}
	warmResp, err := m.client.Do(warmReq)
	if err != nil {
		return TokenResult{}, fmt.Errorf("warm accounts: %w", err)
	}
	warmResp.Body.Close()
	if strings.Contains(warmResp.Request.URL.Path, "sign-in") || strings.Contains(warmResp.Request.URL.Path, "sign-up") {
		return TokenResult{}, fmt.Errorf("sso invalid")
	}

	device, err := m.requestDeviceCode(ctx)
	if err != nil {
		return TokenResult{}, err
	}
	if err := m.verifyAndApprove(ctx, device); err != nil {
		return TokenResult{}, err
	}
	token, err := m.pollToken(ctx, device)
	if err != nil {
		return TokenResult{}, err
	}
	if email == "" {
		email = emailFromJWT(token.AccessToken)
	}
	token.Email = email
	if token.UserID == "" {
		token.UserID = subFromJWT(token.AccessToken)
	}
	return token, nil
}

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func (m *DeviceMinter) requestDeviceCode(ctx context.Context) (deviceCodeResponse, error) {
	form := url.Values{
		"client_id": {ClientID},
		"scope":     {Scopes},
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		Issuer+"/oauth2/device/code",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return deviceCodeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		return deviceCodeResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return deviceCodeResponse{}, fmt.Errorf("device/code HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var decoded deviceCodeResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return deviceCodeResponse{}, err
	}
	if decoded.DeviceCode == "" || decoded.UserCode == "" {
		return deviceCodeResponse{}, fmt.Errorf("device/code missing fields")
	}
	if decoded.Interval <= 0 {
		decoded.Interval = 5
	}
	if decoded.ExpiresIn <= 0 {
		decoded.ExpiresIn = 1800
	}
	return decoded, nil
}

func (m *DeviceMinter) verifyAndApprove(ctx context.Context, device deviceCodeResponse) error {
	verifyURL := device.VerificationURIComplete
	if verifyURL == "" {
		verifyURL = strings.TrimRight(device.VerificationURI, "/") + "?user_code=" + url.QueryEscape(device.UserCode)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("open verification uri: %w", err)
	}
	resp.Body.Close()

	form := url.Values{"user_code": {device.UserCode}}
	verifyReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		Issuer+"/oauth2/device/verify",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return err
	}
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	verifyResp, err := m.client.Do(verifyReq)
	if err != nil {
		return fmt.Errorf("device/verify: %w", err)
	}
	verifyResp.Body.Close()
	if !strings.Contains(verifyResp.Request.URL.Path, "consent") &&
		!strings.Contains(verifyResp.Request.URL.String(), "consent") {
		// Some flows land differently; continue to approve.
	}

	approveForm := url.Values{
		"user_code":      {device.UserCode},
		"action":         {"allow"},
		"principal_type": {"User"},
		"principal_id":   {""},
	}
	approveReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		Issuer+"/oauth2/device/approve",
		strings.NewReader(approveForm.Encode()),
	)
	if err != nil {
		return err
	}
	approveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	approveResp, err := m.client.Do(approveReq)
	if err != nil {
		return fmt.Errorf("device/approve: %w", err)
	}
	approveResp.Body.Close()
	return nil
}

func (m *DeviceMinter) pollToken(ctx context.Context, device deviceCodeResponse) (TokenResult, error) {
	deadline := time.Now().Add(time.Duration(min(device.ExpiresIn, 90)) * time.Second)
	interval := time.Duration(device.Interval) * time.Second
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return TokenResult{}, err
		}
		select {
		case <-ctx.Done():
			return TokenResult{}, ctx.Err()
		case <-time.After(interval):
		}
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {ClientID},
			"device_code": {device.DeviceCode},
		}
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			Issuer+"/oauth2/token",
			strings.NewReader(form.Encode()),
		)
		if err != nil {
			return TokenResult{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := m.client.Do(req)
		if err != nil {
			return TokenResult{}, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			var errBody struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &errBody)
			switch errBody.Error {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			default:
				return TokenResult{}, fmt.Errorf("token poll: %s", firstNonEmpty(errBody.Error, truncate(string(raw), 200)))
			}
		}
		var token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		}
		if err := json.Unmarshal(raw, &token); err != nil {
			return TokenResult{}, err
		}
		if strings.TrimSpace(token.AccessToken) == "" {
			return TokenResult{}, fmt.Errorf("token response missing access_token")
		}
		if token.ExpiresIn <= 0 {
			token.ExpiresIn = 21600
		}
		return TokenResult{
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			ExpiresIn:    token.ExpiresIn,
			UserID:       subFromJWT(token.AccessToken),
			Email:        emailFromJWT(token.AccessToken),
		}, nil
	}
	return TokenResult{}, fmt.Errorf("device token poll timeout")
}

func (t TokenResult) ToImportAccount() admin.ImportAccount {
	return admin.ImportAccount{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresIn:    t.ExpiresIn,
		Email:        t.Email,
		UserID:       t.UserID,
		OIDCIssuer:   Issuer,
		OIDCClientID: ClientID,
	}
}

func subFromJWT(token string) string {
	payload := jwtPayload(token)
	if sub, _ := payload["sub"].(string); sub != "" {
		return sub
	}
	if principal, _ := payload["principal_id"].(string); principal != "" {
		return principal
	}
	return ""
}

func emailFromJWT(token string) string {
	payload := jwtPayload(token)
	if email, _ := payload["email"].(string); email != "" {
		return email
	}
	return ""
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		padded := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
		raw, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return map[string]any{}
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return map[string]any{}
	}
	return payload
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
