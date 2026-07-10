package register

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/register/codec"
	"github.com/AokiAx/grok2api/internal/register/turnstile"
)

const (
	grpcService      = "auth_mgmt.AuthManagement"
	defaultStateTree = "%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D"
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
)

var allowedSSOHosts = map[string]struct{}{
	"auth.x.ai":                {},
	"auth.grok.com":            {},
	"auth.grokipedia.com":      {},
	"auth.grokusercontent.com": {},
	"accounts.x.ai":            {},
}

type RegistrarConfig struct {
	AccountsBase     string
	TurnstileSitekey string
	ProxyURL         string
	Solver           turnstile.Solver
	TurnstileTimeout time.Duration
	HTTPClient       *http.Client
}

type Registrar struct {
	accountsBase        string
	turnstileSitekey    string
	turnstileWebsiteURL string
	stateTree           string
	proxyURL            string
	solver              turnstile.Solver
	turnstileTimeout    time.Duration
	client              *http.Client
}

type RegistrationResult struct {
	StatusCode  int
	SSO         string
	SSORW       string
	VerifyURL   string
	ActionError string
	BodySnippet string
}

func NewRegistrar(cfg RegistrarConfig) (*Registrar, error) {
	if strings.TrimSpace(cfg.AccountsBase) == "" {
		cfg.AccountsBase = "https://accounts.x.ai"
	}
	if cfg.HTTPClient == nil {
		jar, _ := cookiejar.New(nil)
		transport := http.DefaultTransport
		if cfg.ProxyURL != "" {
			proxyURL, err := url.Parse(cfg.ProxyURL)
			if err != nil {
				return nil, fmt.Errorf("parse proxy: %w", err)
			}
			transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}
		cfg.HTTPClient = &http.Client{
			Timeout:   60 * time.Second,
			Jar:       jar,
			Transport: transport,
		}
	} else if cfg.HTTPClient.Jar == nil {
		jar, _ := cookiejar.New(nil)
		clone := *cfg.HTTPClient
		clone.Jar = jar
		cfg.HTTPClient = &clone
	}
	if cfg.TurnstileTimeout <= 0 {
		cfg.TurnstileTimeout = 120 * time.Second
	}
	return &Registrar{
		accountsBase:        strings.TrimRight(cfg.AccountsBase, "/"),
		turnstileSitekey:    cfg.TurnstileSitekey,
		turnstileWebsiteURL: strings.TrimRight(cfg.AccountsBase, "/") + "/sign-up?redirect=grok-com",
		proxyURL:            cfg.ProxyURL,
		solver:              cfg.Solver,
		turnstileTimeout:    cfg.TurnstileTimeout,
		client:              cfg.HTTPClient,
	}, nil
}

func (r *Registrar) Register(
	ctx context.Context,
	email, password, givenName, familyName, mailToken string,
	waitCode func(context.Context, string, string) (string, error),
) (RegistrationResult, error) {
	actionID, err := r.visitSignupPage(ctx)
	if err != nil {
		return RegistrationResult{}, err
	}
	if err := r.createEmailValidationCode(ctx, email); err != nil {
		return RegistrationResult{}, err
	}
	code, err := waitCode(ctx, mailToken, email)
	if err != nil {
		return RegistrationResult{}, fmt.Errorf("wait email code: %w", err)
	}
	code = strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(code, "-", ""), " ", ""))
	if len(code) != 6 {
		return RegistrationResult{}, fmt.Errorf("invalid email code %q", code)
	}
	if err := r.verifyEmailValidationCode(ctx, email, code); err != nil {
		return RegistrationResult{}, err
	}
	token, err := r.solveTurnstile(ctx)
	if err != nil {
		return RegistrationResult{}, err
	}
	result, err := r.submitRegistration(ctx, email, password, givenName, familyName, code, actionID, token)
	if err != nil {
		return RegistrationResult{}, err
	}
	if result.StatusCode == 200 && result.SSO == "" {
		fallbackToken, solveErr := r.solveTurnstile(ctx)
		if solveErr == nil {
			if setter, createErr := r.createSession(ctx, email, password, fallbackToken); createErr == nil {
				cookies := r.followSSOCookieChainURL(ctx, setter)
				result.SSO = cookies["sso"]
				result.SSORW = cookies["sso-rw"]
			}
		}
	}
	if result.SSO == "" {
		return result, fmt.Errorf("registration completed without sso cookie (status=%d error=%s)", result.StatusCode, result.ActionError)
	}
	return result, nil
}

func (r *Registrar) visitSignupPage(ctx context.Context) (string, error) {
	pageURL := r.accountsBase + "/sign-up?redirect=grok-com"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	r.setCommonHeaders(req)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Referer", "https://grok.com/")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("visit signup: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	html := string(body)
	if resp.Request != nil && resp.Request.URL != nil {
		r.turnstileWebsiteURL = resp.Request.URL.String()
	}
	actionID := r.extractActionIDFromChunks(ctx, html, r.turnstileWebsiteURL)
	if actionID == "" {
		actionID = codec.ExtractServerActionIDFromHTML(html)
	}
	if actionID == "" {
		return "", fmt.Errorf("failed to resolve next-action id")
	}
	if match := regexp.MustCompile(`sitekey["\s:]+["']?(0x[0-9A-Za-z]+)`).FindStringSubmatch(html); len(match) == 2 {
		r.turnstileSitekey = match[1]
	}
	if match := regexp.MustCompile(`next-router-state-tree":"([^"]+)"`).FindStringSubmatch(html); len(match) == 2 {
		r.stateTree = match[1]
	}
	// warm root for clean cf cookie
	warm, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.accountsBase, nil)
	if warm != nil {
		r.setCommonHeaders(warm)
		warm.Header.Set("Accept", "text/html,*/*;q=0.8")
		if warmResp, warmErr := r.client.Do(warm); warmErr == nil {
			warmResp.Body.Close()
		}
	}
	return actionID, nil
}

func (r *Registrar) extractActionIDFromChunks(ctx context.Context, pageHTML, pageURL string) string {
	matches := regexp.MustCompile(`<script[^>]+src="(/_next/static/chunks/[^"]+\.js)"`).FindAllStringSubmatch(pageHTML, -1)
	seen := map[string]struct{}{}
	for i := len(matches) - 1; i >= 0; i-- {
		path := matches[i][1]
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.accountsBase+path, nil)
		if err != nil {
			continue
		}
		r.setCommonHeaders(req)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Referer", pageURL)
		resp, err := r.client.Do(req)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		if id := codec.ExtractServerActionIDFromJS(string(raw)); id != "" {
			return id
		}
	}
	return ""
}

func (r *Registrar) createEmailValidationCode(ctx context.Context, email string) error {
	payload := codec.EncodeStringField(1, email)
	return r.grpcCall(ctx, "CreateEmailValidationCode", payload)
}

func (r *Registrar) verifyEmailValidationCode(ctx context.Context, email, code string) error {
	payload := append(codec.EncodeStringField(1, email), codec.EncodeStringField(2, code)...)
	return r.grpcCall(ctx, "VerifyEmailValidationCode", payload)
}

func (r *Registrar) grpcCall(ctx context.Context, method string, payload []byte) error {
	body := codec.WrapGRPCWeb(payload)
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/%s/%s", r.accountsBase, grpcService, method),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	r.setCommonHeaders(req)
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("x-grpc-web", "1")
	req.Header.Set("x-user-agent", "connect-es/2.1.1")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", r.accountsBase)
	req.Header.Set("Referer", r.accountsBase+"/sign-up?redirect=grok-com")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("grpc %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("grpc %s HTTP %d", method, resp.StatusCode)
	}
	parsed := codec.ParseGRPCWebResponse(raw)
	if parsed.Status != "" && parsed.Status != "0" {
		msg := parsed.Trailers["grpc-message"]
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("grpc %s error: %s", method, msg)
	}
	return nil
}

func (r *Registrar) solveTurnstile(ctx context.Context) (string, error) {
	if r.solver == nil {
		return "", fmt.Errorf("turnstile solver not configured")
	}
	solveCtx, cancel := context.WithTimeout(ctx, r.turnstileTimeout)
	defer cancel()
	return r.solver.Solve(solveCtx, r.turnstileSitekey, r.turnstileWebsiteURL)
}

func (r *Registrar) submitRegistration(
	ctx context.Context,
	email, password, givenName, familyName, emailCode, actionID, turnstileToken string,
) (RegistrationResult, error) {
	if !codec.ActionIDPattern.MatchString(actionID) {
		return RegistrationResult{}, fmt.Errorf("invalid next-action id")
	}
	payload := []map[string]any{{
		"emailValidationCode": emailCode,
		"createUserAndSessionRequest": map[string]any{
			"email":              email,
			"givenName":          givenName,
			"familyName":         familyName,
			"clearTextPassword":  password,
			"tosAcceptedVersion": "$undefined",
		},
		"turnstileToken":         turnstileToken,
		"promptOnDuplicateEmail": true,
	}}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.accountsBase+"/sign-up", bytes.NewReader(body))
	if err != nil {
		return RegistrationResult{}, err
	}
	tree := r.stateTree
	if tree == "" {
		tree = defaultStateTree
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/x-component")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", r.accountsBase)
	req.Header.Set("Referer", r.accountsBase+"/sign-up")
	req.Header.Set("next-router-state-tree", tree)
	req.Header.Set("next-action", actionID)
	if cf := r.cookieValue("__cf_bm"); cf != "" {
		req.Header.Set("Cookie", "__cf_bm="+cf)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return RegistrationResult{}, fmt.Errorf("submit registration: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	text := string(raw)
	verifyURL, actionError := parseSignupActionResponse(text)
	result := RegistrationResult{
		StatusCode:  resp.StatusCode,
		VerifyURL:   verifyURL,
		ActionError: actionError,
		BodySnippet: truncate(text, 2000),
	}
	if verifyURL != "" {
		cookies := r.followSSOCookieChainURL(ctx, verifyURL)
		result.SSO = cookies["sso"]
		result.SSORW = cookies["sso-rw"]
	}
	if result.SSO == "" {
		result.SSO = r.cookieValue("sso")
		result.SSORW = r.cookieValue("sso-rw")
	}
	return result, nil
}

func parseSignupActionResponse(text string) (verifyURL, actionError string) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, ":") {
			continue
		}
		_, payload, _ := strings.Cut(line, ":")
		payload = strings.TrimSpace(payload)
		if !strings.HasPrefix(payload, "{") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if urlVal, ok := obj["url"].(string); ok && urlVal != "" {
			verifyURL = strings.ReplaceAll(urlVal, "\\/", "/")
		}
		if errVal, ok := obj["error"].(string); ok && errVal != "" {
			actionError = errVal
		}
	}
	if verifyURL == "" {
		if match := regexp.MustCompile(`(https://[^"\s]+set-cookie\?q=[^:"\s]+)`).FindStringSubmatch(text); len(match) == 2 {
			verifyURL = strings.ReplaceAll(match[1], "\\/", "/")
		}
	}
	return verifyURL, actionError
}

func (r *Registrar) createSession(ctx context.Context, email, password, turnstileToken string) (string, error) {
	payload := map[string]any{
		"rpc": "createSession",
		"req": map[string]any{
			"createSessionRequest": map[string]any{
				"credentials": map[string]any{
					"case": "emailAndPassword",
					"value": map[string]any{
						"email":             email,
						"clearTextPassword": password,
					},
				},
			},
			"turnstileToken": turnstileToken,
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.accountsBase+"/api/rpc", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	r.setCommonHeaders(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.accountsBase)
	req.Header.Set("Referer", r.accountsBase+"/sign-up?redirect=grok-com")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("createSession HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var decoded struct {
		CookieSetterURL string `json:"cookieSetterUrl"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	if !strings.HasPrefix(decoded.CookieSetterURL, "https://auth.") {
		return "", fmt.Errorf("unexpected cookieSetterUrl")
	}
	return decoded.CookieSetterURL, nil
}

func (r *Registrar) followSSOCookieChainURL(ctx context.Context, entry string) map[string]string {
	collected := map[string]string{}
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return collected
	}
	// preferred decoded hop
	for _, candidate := range deriveSSOChainURLs(entry) {
		parsed, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		if strings.HasSuffix(parsed.Host, "x.ai") && strings.HasSuffix(parsed.Path, "/set-cookie") {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate, nil)
			if err != nil {
				break
			}
			r.setSSOHeaders(req)
			resp, err := r.client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			if sso := r.cookieValue("sso"); sso != "" {
				collected["sso"] = sso
				collected["sso-rw"] = r.cookieValue("sso-rw")
				return collected
			}
			break
		}
	}
	// auto redirect
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry, nil)
	if err == nil {
		r.setSSOHeaders(req)
		if resp, doErr := r.client.Do(req); doErr == nil {
			resp.Body.Close()
		}
	}
	if sso := r.cookieValue("sso"); sso != "" {
		collected["sso"] = sso
		collected["sso-rw"] = r.cookieValue("sso-rw")
		return collected
	}
	// manual hops
	current := entry
	for i := 0; i < 8; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			break
		}
		r.setSSOHeaders(req)
		// prevent auto follow to inspect location
		client := *r.client
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err := client.Do(req)
		if err != nil {
			break
		}
		for _, cookie := range resp.Cookies() {
			if cookie.Name == "sso" || cookie.Name == "sso-rw" {
				collected[cookie.Name] = cookie.Value
			}
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if location == "" {
				break
			}
			if strings.HasPrefix(location, "/") {
				parsed, _ := url.Parse(current)
				location = parsed.Scheme + "://" + parsed.Host + location
			}
			host := ""
			if parsed, err := url.Parse(location); err == nil {
				host = parsed.Host
			}
			if _, ok := allowedSSOHosts[host]; !ok {
				break
			}
			current = location
			continue
		}
		resp.Body.Close()
		break
	}
	if collected["sso"] == "" {
		if sso := r.cookieValue("sso"); sso != "" {
			collected["sso"] = sso
			collected["sso-rw"] = r.cookieValue("sso-rw")
		}
	}
	return collected
}

func deriveSSOChainURLs(entry string) []string {
	chain := []string{}
	current := strings.TrimSpace(entry)
	seen := map[string]struct{}{}
	for i := 0; i < 8; i++ {
		if current == "" {
			break
		}
		if _, ok := seen[current]; ok {
			break
		}
		parsed, err := url.Parse(current)
		if err != nil {
			break
		}
		if _, ok := allowedSSOHosts[parsed.Host]; !ok {
			break
		}
		seen[current] = struct{}{}
		chain = append(chain, current)
		next := decodeSSOSuccessURL(current)
		if next == "" {
			break
		}
		current = next
	}
	return chain
}

func decodeSSOSuccessURL(setCookieURL string) string {
	parsed, err := url.Parse(setCookieURL)
	if err != nil {
		return ""
	}
	q := parsed.Query().Get("q")
	parts := strings.Split(q, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		padded := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
		raw, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return ""
		}
	}
	var obj struct {
		Config struct {
			SuccessURL string `json:"success_url"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return obj.Config.SuccessURL
}

func (r *Registrar) cookieValue(name string) string {
	if r.client.Jar == nil {
		return ""
	}
	accountsURL, _ := url.Parse(r.accountsBase + "/")
	for _, cookie := range r.client.Jar.Cookies(accountsURL) {
		if cookie.Name == name && cookie.Value != "" {
			return cookie.Value
		}
	}
	// also check auth.x.ai jar scope via synthetic URL
	authURL, _ := url.Parse("https://auth.x.ai/")
	for _, cookie := range r.client.Jar.Cookies(authURL) {
		if cookie.Name == name && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

func (r *Registrar) setCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("sec-ch-ua", `"Not:A-Brand";v="99", "Google Chrome";v="136", "Chromium";v="136"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
}

func (r *Registrar) setSSOHeaders(req *http.Request) {
	r.setCommonHeaders(req)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Referer", r.accountsBase+"/")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
