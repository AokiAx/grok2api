package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
)

// ClientOptions configures CLI fingerprint headers that mirror the official
// Grok Build / CLI client surface.
type ClientOptions struct {
	// TokenAuth is X-XAI-Token-Auth (default xai-grok-cli).
	TokenAuth string
	// ClientIdentifier is x-grok-client-identifier / x-grok-client-name.
	ClientIdentifier string
	// UserAgent overrides User-Agent; empty uses xai-grok-build/<version>.
	UserAgent string
}

type clientIdentity struct {
	agentID   string
	sessionID string
}

type Client struct {
	baseURL          string
	clientVersion    string
	tokenAuth        string
	clientIdentifier string
	userAgent        string
	httpClient       *http.Client
	streamHTTPClient *http.Client
	discoveryMu      sync.Mutex
	tokenEndpoints   map[string]string
	identityMu       sync.Mutex
	identities       map[string]clientIdentity
}

func NewClient(baseURL, clientVersion string, httpClient *http.Client) *Client {
	return NewClientWithOptions(baseURL, clientVersion, httpClient, ClientOptions{})
}

func NewClientWithOptions(baseURL, clientVersion string, httpClient *http.Client, opts ClientOptions) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	streamHTTPClient := *httpClient
	streamHTTPClient.Timeout = 0
	tokenAuth := strings.TrimSpace(opts.TokenAuth)
	if tokenAuth == "" {
		tokenAuth = "xai-grok-cli"
	}
	identifier := strings.TrimSpace(opts.ClientIdentifier)
	if identifier == "" {
		identifier = "grok-cli"
	}
	return &Client{
		baseURL:          strings.TrimRight(baseURL, "/"),
		clientVersion:    clientVersion,
		tokenAuth:        tokenAuth,
		clientIdentifier: identifier,
		userAgent:        strings.TrimSpace(opts.UserAgent),
		httpClient:       httpClient,
		streamHTTPClient: &streamHTTPClient,
		tokenEndpoints:   make(map[string]string),
		identities:       make(map[string]clientIdentity),
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
	if err := c.applyCLIHeaders(request, item, model, ConvIDFrom(ctx), true); err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if stream {
		// Match official CLI streaming: avoid opaque gzip on SSE bodies.
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("Accept-Encoding", "identity")
	} else {
		request.Header.Set("Accept", "application/json")
	}

	client := c.httpClient
	if stream && c.streamHTTPClient != nil {
		client = c.streamHTTPClient
	}
	response, err := client.Do(request)
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
	// New free accounts frequently return transient permission-denied on the
	// first /responses call right after mint. Retry a couple of times before
	// letting the caller park the account as validating.
	const attempts = 3
	var lastReason account.UnavailableReason
	var lastCode string
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return account.ReasonValidating, "probe-cancelled", nil
			case <-time.After(1500 * time.Millisecond):
			}
		}
		reason, code, err := c.validateResponsesProbeOnce(ctx, item)
		if err != nil {
			return "", "", err
		}
		if reason == "" {
			return "", "", nil
		}
		lastReason, lastCode = reason, code
		// Only retry provisioning-style denials; hard auth/quota fail fast.
		if reason != account.ReasonValidating && !strings.EqualFold(code, "permission-denied") {
			return reason, code, nil
		}
	}
	return lastReason, lastCode, nil
}

func (c *Client) validateResponsesProbeOnce(
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
	// Probes use the same CLI fingerprint as live traffic (minus per-request trace).
	_ = c.applyCLIHeaders(request, item, model, "", false)
}

// applyCLIHeaders writes the Grok Build CLI header surface used by
// cli-chat-proxy .
func (c *Client) applyCLIHeaders(
	request *http.Request,
	item account.Account,
	model string,
	conversationID string,
	trace bool,
) error {
	if request == nil {
		return nil
	}
	identity, err := c.clientIdentity(item.ID)
	if err != nil {
		return err
	}
	requestID, err := randomHex(16)
	if err != nil {
		return err
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		conversationID, err = randomHex(16)
		if err != nil {
			return err
		}
	}

	request.Header.Set("Authorization", "Bearer "+item.AccessToken)
	request.Header.Set("X-XAI-Token-Auth", c.tokenAuthValue())
	request.Header.Set("x-grok-client-version", c.clientVersion)
	request.Header.Set("x-grok-client-identifier", c.clientIdentifierValue())
	request.Header.Set("x-grok-client-surface", "tui")
	request.Header.Set("x-grok-client-name", c.clientIdentifierValue())
	request.Header.Set("x-grok-agent-id", identity.agentID)
	request.Header.Set("x-grok-session-id", identity.sessionID)
	request.Header.Set("x-grok-session-id-legacy", identity.sessionID)
	request.Header.Set("x-grok-conv-id", conversationID)
	request.Header.Set("x-grok-conversation-id", conversationID)
	request.Header.Set("x-grok-req-id", requestID)
	request.Header.Set("x-grok-request-id", requestID)
	if userID := strings.TrimSpace(item.UserID); userID != "" {
		request.Header.Set("x-userid", userID)
	}
	request.Header.Set("User-Agent", c.userAgentValue())
	if model != "" {
		request.Header.Set("x-grok-model-override", model)
	}
	if trace {
		traceID, err := randomHex(16)
		if err != nil {
			return err
		}
		spanID, err := randomHex(8)
		if err != nil {
			return err
		}
		request.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
		request.Header.Set("tracestate", "")
	}
	return nil
}

func (c *Client) clientIdentity(accountID string) (clientIdentity, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		// Ephemeral identity for probes without a stable account id.
		agentID, err := randomHex(16)
		if err != nil {
			return clientIdentity{}, err
		}
		sessionID, err := randomUUID()
		if err != nil {
			return clientIdentity{}, err
		}
		return clientIdentity{agentID: agentID, sessionID: sessionID}, nil
	}
	c.identityMu.Lock()
	defer c.identityMu.Unlock()
	if value, ok := c.identities[accountID]; ok {
		return value, nil
	}
	agentID, err := randomHex(16)
	if err != nil {
		return clientIdentity{}, err
	}
	sessionID, err := randomUUID()
	if err != nil {
		return clientIdentity{}, err
	}
	value := clientIdentity{agentID: agentID, sessionID: sessionID}
	c.identities[accountID] = value
	return value, nil
}

func (c *Client) tokenAuthValue() string {
	if c != nil && strings.TrimSpace(c.tokenAuth) != "" {
		return c.tokenAuth
	}
	return "xai-grok-cli"
}

func (c *Client) clientIdentifierValue() string {
	if c != nil && strings.TrimSpace(c.clientIdentifier) != "" {
		return c.clientIdentifier
	}
	return "grok-cli"
}

func (c *Client) userAgentValue() string {
	if c != nil && strings.TrimSpace(c.userAgent) != "" {
		return c.userAgent
	}
	version := "0.0.0"
	if c != nil && strings.TrimSpace(c.clientVersion) != "" {
		version = c.clientVersion
	}
	return "xai-grok-build/" + version
}

func randomHex(bytesLength int) (string, error) {
	value := make([]byte, bytesLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:], nil
}

// PermanentRefreshError means the refresh token is dead and should not be retried.
type PermanentRefreshError struct {
	Code    string
	Message string
}

func (e *PermanentRefreshError) Error() string {
	if e == nil {
		return "permanent refresh error"
	}
	if e.Code != "" && e.Message != "" {
		return e.Code + ": " + e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	if e.Message != "" {
		return e.Message
	}
	return "permanent refresh error"
}

func (e *PermanentRefreshError) Permanent() bool { return true }

func IsPermanentRefreshError(err error) bool {
	var target *PermanentRefreshError
	return errors.As(err, &target)
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
	body, err := io.ReadAll(io.LimitReader(tokenResponse.Body, 1<<20))
	if err != nil {
		return account.Account{}, fmt.Errorf("read OIDC refresh response: %w", err)
	}
	if tokenResponse.StatusCode >= http.StatusBadRequest {
		return account.Account{}, classifyRefreshFailure(tokenResponse.StatusCode, body)
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    any    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
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

func classifyRefreshFailure(status int, body []byte) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)
	code := strings.ToLower(strings.TrimSpace(payload.Error))
	desc := strings.ToLower(strings.TrimSpace(payload.ErrorDescription))
	// invalid_grant / revoked / expired refresh tokens are permanent for this credential.
	if code == "invalid_grant" ||
		strings.Contains(desc, "revoked") ||
		strings.Contains(desc, "expired") ||
		strings.Contains(desc, "invalid refresh") {
		msg := firstNonEmpty(payload.ErrorDescription, payload.Error, fmt.Sprintf("OIDC refresh returned %d", status))
		return &PermanentRefreshError{Code: firstNonEmpty(payload.Error, "invalid_grant"), Message: msg}
	}
	return fmt.Errorf("OIDC refresh returned %d", status)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
