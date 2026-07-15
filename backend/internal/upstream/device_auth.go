package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeviceAuthorization is the public result of starting a device auth flow.
// DeviceCode is for server-side polling only and must not be logged or returned
// to browser clients.
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
	Interval                time.Duration
}

// DeviceTokenResult is the token endpoint result for a device code poll.
type DeviceTokenResult struct {
	AccessToken      string
	RefreshToken     string
	ExpiresIn        time.Duration
	TokenType        string
	Pending          bool
	SlowDown         bool
	Denied           bool
	Expired          bool
	Error            string
	ErrorDescription string
}

func (c *Client) StartDeviceAuthorization(ctx context.Context, issuer, clientID, scope string) (DeviceAuthorization, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	clientID = strings.TrimSpace(clientID)
	if issuer == "" || clientID == "" {
		return DeviceAuthorization{}, fmt.Errorf("issuer and client_id are required")
	}
	deviceEndpoint, err := c.discoverDeviceEndpoint(ctx, issuer)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	form := url.Values{"client_id": {clientID}}
	if strings.TrimSpace(scope) != "" {
		form.Set("scope", strings.TrimSpace(scope))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("create device authorization request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("device authorization request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("read device authorization response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		if snippet == "" {
			return DeviceAuthorization{}, fmt.Errorf("device authorization returned %d", resp.StatusCode)
		}
		return DeviceAuthorization{}, fmt.Errorf("device authorization returned %d: %s", resp.StatusCode, snippet)
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               any    `json:"expires_in"`
		Interval                any    `json:"interval"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return DeviceAuthorization{}, fmt.Errorf("decode device authorization response: %w", err)
	}
	if strings.TrimSpace(payload.DeviceCode) == "" || strings.TrimSpace(payload.UserCode) == "" {
		return DeviceAuthorization{}, fmt.Errorf("device authorization missing codes")
	}
	if strings.TrimSpace(payload.VerificationURI) == "" {
		return DeviceAuthorization{}, fmt.Errorf("device authorization missing verification_uri")
	}
	return DeviceAuthorization{
		DeviceCode:              strings.TrimSpace(payload.DeviceCode),
		UserCode:                strings.TrimSpace(payload.UserCode),
		VerificationURI:         strings.TrimSpace(payload.VerificationURI),
		VerificationURIComplete: strings.TrimSpace(payload.VerificationURIComplete),
		ExpiresIn:               secondsValue(payload.ExpiresIn, 15*time.Minute),
		Interval:                secondsValue(payload.Interval, 5*time.Second),
	}, nil
}

func (c *Client) PollDeviceToken(ctx context.Context, issuer, clientID, deviceCode string) (DeviceTokenResult, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	clientID = strings.TrimSpace(clientID)
	deviceCode = strings.TrimSpace(deviceCode)
	if issuer == "" || clientID == "" || deviceCode == "" {
		return DeviceTokenResult{}, fmt.Errorf("issuer, client_id and device_code are required")
	}
	tokenEndpoint, err := c.discoverTokenEndpoint(ctx, issuer)
	if err != nil {
		return DeviceTokenResult{}, err
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceTokenResult{}, fmt.Errorf("create device token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return DeviceTokenResult{}, fmt.Errorf("device token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeviceTokenResult{}, fmt.Errorf("read device token response: %w", err)
	}
	var payload struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        any    `json:"expires_in"`
		TokenType        string `json:"token_type"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)
	errCode := strings.ToLower(strings.TrimSpace(payload.Error))
	if resp.StatusCode >= http.StatusBadRequest || errCode != "" {
		switch errCode {
		case "authorization_pending":
			return DeviceTokenResult{Pending: true, Error: errCode, ErrorDescription: payload.ErrorDescription}, nil
		case "slow_down":
			return DeviceTokenResult{Pending: true, SlowDown: true, Error: errCode, ErrorDescription: payload.ErrorDescription}, nil
		case "access_denied":
			return DeviceTokenResult{Denied: true, Error: errCode, ErrorDescription: payload.ErrorDescription}, nil
		case "expired_token":
			return DeviceTokenResult{Expired: true, Error: errCode, ErrorDescription: payload.ErrorDescription}, nil
		default:
			if errCode == "" {
				errCode = fmt.Sprintf("http_%d", resp.StatusCode)
			}
			return DeviceTokenResult{Error: errCode, ErrorDescription: payload.ErrorDescription}, fmt.Errorf("device token returned %s", errCode)
		}
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return DeviceTokenResult{}, fmt.Errorf("device token response missing access_token")
	}
	return DeviceTokenResult{
		AccessToken:  strings.TrimSpace(payload.AccessToken),
		RefreshToken: strings.TrimSpace(payload.RefreshToken),
		ExpiresIn:    secondsValue(payload.ExpiresIn, 6*time.Hour),
		TokenType:    strings.TrimSpace(payload.TokenType),
	}, nil
}

func (c *Client) discoverDeviceEndpoint(ctx context.Context, issuer string) (string, error) {
	c.discoveryMu.Lock()
	if c.deviceEndpoints != nil {
		if endpoint := c.deviceEndpoints[issuer]; endpoint != "" {
			c.discoveryMu.Unlock()
			return endpoint, nil
		}
	}
	c.discoveryMu.Unlock()

	discoveryURL := issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("create OIDC discovery request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discover OIDC endpoints: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("OIDC discovery returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var payload struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode OIDC discovery: %w", err)
	}
	endpoint := strings.TrimSpace(payload.DeviceAuthorizationEndpoint)
	if endpoint == "" {
		endpoint = strings.TrimRight(issuer, "/") + "/oauth2/device/code"
	}
	c.discoveryMu.Lock()
	if c.deviceEndpoints == nil {
		c.deviceEndpoints = make(map[string]string)
	}
	c.deviceEndpoints[issuer] = endpoint
	c.discoveryMu.Unlock()
	return endpoint, nil
}
