package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
)

type Client struct {
	baseURL        string
	clientVersion  string
	httpClient     *http.Client
	discoveryMu    sync.Mutex
	tokenEndpoints map[string]string
}

func NewClient(baseURL, clientVersion string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		clientVersion:  clientVersion,
		httpClient:     httpClient,
		tokenEndpoints: make(map[string]string),
	}
}

func (c *Client) Chat(
	ctx context.Context,
	item account.Account,
	payload []byte,
	stream bool,
) (*http.Response, error) {
	return c.Request(ctx, item, http.MethodPost, "/chat/completions", payload, stream)
}

// ProbeFreeQuota checks whether a free account still has usable capacity.
// Free tier has no reliable billing query. Production chat for grok-4.5 goes
// through /responses (prompt-cache friendly), so recovery probes the same path.
func (c *Client) ProbeFreeQuota(
	ctx context.Context,
	item account.Account,
) (account.UnavailableReason, string, error) {
	reason, code, _, _, _, err := c.ProbeFreeQuotaUsage(ctx, item)
	return reason, code, err
}

// ProbeFreeQuotaUsage is like ProbeFreeQuota but also returns rate-limit usage
// observed on the probe response for persistence.
func (c *Client) ProbeFreeQuotaUsage(
	ctx context.Context,
	item account.Account,
) (account.UnavailableReason, string, int64, int64, bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Minimal responses stream probe — same surface as live chat backend.
	payload := []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"ping"}],"stream":true,"max_output_tokens":8}`)
	response, err := c.Request(probeCtx, item, http.MethodPost, "/responses", payload, true)
	if err != nil {
		return "", "", 0, 0, false, fmt.Errorf("probe free quota: %w", err)
	}
	defer response.Body.Close()
	usage := ParseRateLimitHeaders(response.Header)
	hasUsage := usage.Present()
	actual, limit := usage.QuotaActual(), usage.QuotaLimit()
	if response.StatusCode < 400 {
		// Drain a tiny prefix so headers are complete, then stop.
		_, _ = io.CopyN(io.Discard, response.Body, 256)
		if hasUsage && usage.Exhausted() {
			return account.ReasonQuota, "subscription:free-usage-exhausted", actual, limit, true, nil
		}
		return "", "", actual, limit, hasUsage, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", "", actual, limit, hasUsage, fmt.Errorf("read free quota probe: %w", err)
	}
	failure := ClassifyFailure(response.StatusCode, body)
	if failure.Kind == FailureQuota && failure.QuotaLimit == 0 && hasUsage {
		failure.QuotaActual = actual
		failure.QuotaLimit = limit
	}
	if failure.Reason == "" {
		return account.ReasonValidating, failure.Code, actual, limit, hasUsage, nil
	}
	if failure.QuotaLimit > 0 || failure.QuotaActual > 0 {
		return failure.Reason, failure.Code, failure.QuotaActual, failure.QuotaLimit, true, nil
	}
	return failure.Reason, failure.Code, actual, limit, hasUsage, nil
}

func (c *Client) Request(
	ctx context.Context,
	item account.Account,
	method string,
	path string,
	payload []byte,
	stream bool,
) (*http.Response, error) {
	model := ""
	var requestBody struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(payload, &requestBody)
	model = requestBody.Model
	path = "/" + strings.TrimLeft(path, "/")
	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		method,
		c.baseURL+path,
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("create upstream %s request: %w", path, err)
	}
	request.Header.Set("Authorization", "Bearer "+item.AccessToken)
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", c.clientVersion)
	request.Header.Set("x-grok-model-override", model)
	request.Header.Set("User-Agent", "xai-grok-build/"+c.clientVersion)
	if len(payload) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	} else {
		request.Header.Set("Accept", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send upstream %s request: %w", path, err)
	}
	return response, nil
}

func (c *Client) Validate(
	ctx context.Context,
	item account.Account,
) (account.UnavailableReason, string, error) {
	// Stage 1: lightweight /models auth probe.
	reason, code, err := c.validateModels(ctx, item)
	if err != nil || reason != "" {
		return reason, code, err
	}
	// Stage 2: short /responses probe (same backend as live chat).
	// Transport errors are not "healthy"; leave as validating so callers backoff.
	probeReason, probeCode, probeErr := c.validateResponsesProbe(ctx, item)
	if probeErr != nil {
		return account.ReasonValidating, "probe-transport-error", nil
	}
	if probeReason != "" {
		return probeReason, probeCode, nil
	}
	return "", "", nil
}

func (c *Client) validateModels(
	ctx context.Context,
	item account.Account,
) (account.UnavailableReason, string, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.baseURL+"/models",
		nil,
	)
	if err != nil {
		return "", "", fmt.Errorf("create validation request: %w", err)
	}
	c.setAuthHeaders(request, item, "")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("validate account: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", "", fmt.Errorf("read validation response: %w", err)
	}
	if response.StatusCode < 400 {
		return "", "", nil
	}
	failure := ClassifyFailure(response.StatusCode, body)
	if failure.Reason == "" {
		return account.ReasonValidating, failure.Code, nil
	}
	return failure.Reason, failure.Code, nil
}

func (c *Client) validateResponsesProbe(
	ctx context.Context,
	item account.Account,
) (account.UnavailableReason, string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	payload := []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"ping"}],"stream":true,"max_output_tokens":16}`)
	request, err := http.NewRequestWithContext(
		probeCtx,
		http.MethodPost,
		c.baseURL+"/responses",
		bytes.NewReader(payload),
	)
	if err != nil {
		return "", "", fmt.Errorf("create probe request: %w", err)
	}
	c.setAuthHeaders(request, item, "grok-4.5")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("probe account: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 400 {
		// Drain a tiny bit then cancel/close; success means account can infer.
		_, _ = io.CopyN(io.Discard, response.Body, 256)
		return "", "", nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("read probe response: %w", err)
	}
	failure := ClassifyFailure(response.StatusCode, body)
	if failure.Reason == "" {
		return account.ReasonValidating, failure.Code, nil
	}
	return failure.Reason, failure.Code, nil
}

func (c *Client) setAuthHeaders(request *http.Request, item account.Account, model string) {
	request.Header.Set("Authorization", "Bearer "+item.AccessToken)
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", c.clientVersion)
	request.Header.Set("User-Agent", "xai-grok-build/"+c.clientVersion)
	if model != "" {
		request.Header.Set("x-grok-model-override", model)
	}
}

func (c *Client) Refresh(ctx context.Context, item account.Account) (account.Account, error) {
	issuer := strings.TrimRight(strings.TrimSpace(item.OIDCIssuer), "/")
	clientID := strings.TrimSpace(item.OIDCClientID)
	refreshToken := strings.TrimSpace(item.RefreshToken)
	if issuer == "" || clientID == "" || refreshToken == "" {
		return account.Account{}, fmt.Errorf("account %s has incomplete OIDC refresh fields", item.ID)
	}
	issuerURL, err := url.Parse(issuer)
	if err != nil || issuerURL.Host == "" || !allowedIssuerScheme(issuerURL) {
		return account.Account{}, fmt.Errorf("account %s has invalid OIDC issuer", item.ID)
	}

	tokenEndpointValue, err := c.discoverTokenEndpoint(ctx, issuer)
	if err != nil {
		return account.Account{}, err
	}
	tokenEndpoint, err := url.Parse(tokenEndpointValue)
	if err != nil || tokenEndpoint.Host == "" || !allowedIssuerScheme(tokenEndpoint) {
		return account.Account{}, fmt.Errorf("OIDC discovery returned invalid token endpoint")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	tokenRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenEndpoint.String(),
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return account.Account{}, fmt.Errorf("create OIDC refresh request: %w", err)
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResponse, err := c.httpClient.Do(tokenRequest)
	if err != nil {
		return account.Account{}, fmt.Errorf("refresh OIDC credential: %w", err)
	}
	defer tokenResponse.Body.Close()
	if tokenResponse.StatusCode >= http.StatusBadRequest {
		return account.Account{}, fmt.Errorf("OIDC refresh returned %d", tokenResponse.StatusCode)
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    any    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(tokenResponse.Body, 1<<20)).Decode(&tokens); err != nil {
		return account.Account{}, fmt.Errorf("decode OIDC refresh response: %w", err)
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return account.Account{}, fmt.Errorf("OIDC refresh returned no access token")
	}
	expiresIn := secondsValue(tokens.ExpiresIn, 6*time.Hour)
	item.AccessToken = strings.TrimSpace(tokens.AccessToken)
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		item.RefreshToken = strings.TrimSpace(tokens.RefreshToken)
	}
	now := time.Now().UTC()
	item.ExpiresAt = now.Add(expiresIn)
	item.UpdatedAt = now
	return item, nil
}

func (c *Client) discoverTokenEndpoint(ctx context.Context, issuer string) (string, error) {
	c.discoveryMu.Lock()
	cached := c.tokenEndpoints[issuer]
	c.discoveryMu.Unlock()
	if cached != "" {
		return cached, nil
	}
	discoveryRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		issuer+"/.well-known/openid-configuration",
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("create OIDC discovery request: %w", err)
	}
	discoveryResponse, err := c.httpClient.Do(discoveryRequest)
	if err != nil {
		return "", fmt.Errorf("discover OIDC token endpoint: %w", err)
	}
	defer discoveryResponse.Body.Close()
	if discoveryResponse.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("OIDC discovery returned %d", discoveryResponse.StatusCode)
	}
	var discovery struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(discoveryResponse.Body, 1<<20)).Decode(&discovery); err != nil {
		return "", fmt.Errorf("decode OIDC discovery: %w", err)
	}
	tokenEndpoint, err := url.Parse(strings.TrimSpace(discovery.TokenEndpoint))
	if err != nil || tokenEndpoint.Host == "" || !allowedIssuerScheme(tokenEndpoint) {
		return "", fmt.Errorf("OIDC discovery returned invalid token endpoint")
	}
	c.discoveryMu.Lock()
	c.tokenEndpoints[issuer] = tokenEndpoint.String()
	c.discoveryMu.Unlock()
	return tokenEndpoint.String(), nil
}

func allowedIssuerScheme(value *url.URL) bool {
	if value.Scheme == "https" {
		return true
	}
	host := value.Hostname()
	return value.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

func secondsValue(value any, fallback time.Duration) time.Duration {
	var seconds int64
	switch typed := value.(type) {
	case float64:
		seconds = int64(typed)
	case string:
		seconds, _ = strconv.ParseInt(typed, 10, 64)
	case json.Number:
		seconds, _ = typed.Int64()
	}
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
