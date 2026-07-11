package turnstile

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

type Solver interface {
	Name() string
	Solve(ctx context.Context, sitekey, pageURL string) (string, error)
	Healthy(ctx context.Context) error
}

type Local struct {
	baseURL string
	client  *http.Client
	timeout time.Duration
}

func NewLocal(baseURL string, timeout time.Duration, client *http.Client) *Local {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Local{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  client,
		timeout: timeout,
	}
}

func (l *Local) Name() string { return "local" }

func (l *Local) Healthy(ctx context.Context) error {
	if l.baseURL == "" {
		return fmt.Errorf("local turnstile solver url empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+"/", nil)
	if err != nil {
		return err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("local solver unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (l *Local) Solve(ctx context.Context, sitekey, pageURL string) (string, error) {
	if l.baseURL == "" {
		return "", fmt.Errorf("local turnstile solver url empty")
	}
	query := url.Values{"url": {pageURL}, "sitekey": {sitekey}}
	createURL := l.baseURL + "/turnstile?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, createURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("local solver create: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("local solver create HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var created struct {
		ErrorID          int    `json:"errorId"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           string `json:"taskId"`
		// Theyka/Turnstile-Solver uses snake_case.
		TaskIDAlt string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", err
	}
	if created.ErrorID != 0 {
		return "", fmt.Errorf("local solver create: %s", created.ErrorDescription)
	}
	taskID := strings.TrimSpace(created.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(created.TaskIDAlt)
	}
	if taskID == "" {
		return "", fmt.Errorf("local solver missing taskId")
	}
	deadline := time.Now().Add(l.timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
		resultURL := l.baseURL + "/result?id=" + url.QueryEscape(taskID)
		resultReq, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
		if err != nil {
			return "", err
		}
		resultReq.Header.Set("Accept", "application/json")
		resultResp, err := l.client.Do(resultReq)
		if err != nil {
			return "", err
		}
		resultRaw, _ := io.ReadAll(io.LimitReader(resultResp.Body, 1<<20))
		resultResp.Body.Close()
		if resultResp.StatusCode >= 400 {
			return "", fmt.Errorf("local solver result HTTP %d", resultResp.StatusCode)
		}
		// Theyka may return plain-text CAPTCHA_NOT_READY before JSON token payload.
		trimmed := strings.TrimSpace(string(resultRaw))
		if trimmed == "" ||
			strings.EqualFold(trimmed, "CAPTCHA_NOT_READY") ||
			strings.EqualFold(trimmed, "processing") ||
			strings.EqualFold(trimmed, "captcha_not_ready") {
			continue
		}
		var result struct {
			ErrorID          int    `json:"errorId"`
			ErrorDescription string `json:"errorDescription"`
			Status           string `json:"status"`
			// CapMonster-style nested token.
			Solution struct {
				Token string `json:"token"`
			} `json:"solution"`
			// Theyka/Turnstile-Solver returns the token in "value".
			Value string `json:"value"`
			// Some forks return a bare token field.
			Token string `json:"token"`
		}
		if err := json.Unmarshal(resultRaw, &result); err != nil {
			// Keep polling on transient non-JSON wait responses.
			if strings.HasPrefix(strings.ToUpper(trimmed), "CAPTCHA") {
				continue
			}
			return "", fmt.Errorf("local solver result parse: %w (%s)", err, truncate(trimmed, 120))
		}
		if result.ErrorID != 0 {
			return "", fmt.Errorf("local solver result: %s", result.ErrorDescription)
		}
		token := strings.TrimSpace(result.Solution.Token)
		if token == "" {
			token = strings.TrimSpace(result.Value)
		}
		if token == "" {
			token = strings.TrimSpace(result.Token)
		}
		if token != "" {
			return token, nil
		}
		switch strings.ToLower(result.Status) {
		case "ready":
			return "", fmt.Errorf("local solver missing token")
		case "processing", "captcha_not_ready", "":
			continue
		default:
			return "", fmt.Errorf("local solver status %q", result.Status)
		}
	}
	return "", fmt.Errorf("local turnstile solve timeout")
}

type CapMonster struct {
	apiBase string
	apiKey  string
	client  *http.Client
	timeout time.Duration
}

func NewCapMonster(apiBase, apiKey string, timeout time.Duration, client *http.Client) *CapMonster {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		apiBase = "https://api.capmonster.cloud"
	}
	return &CapMonster{apiBase: apiBase, apiKey: strings.TrimSpace(apiKey), client: client, timeout: timeout}
}

func (c *CapMonster) Name() string { return "capmonster" }

func (c *CapMonster) Healthy(ctx context.Context) error {
	if c.apiKey == "" {
		return fmt.Errorf("capmonster api key empty")
	}
	_, err := c.post(ctx, "getBalance", map[string]any{"clientKey": c.apiKey})
	return err
}

func (c *CapMonster) Solve(ctx context.Context, sitekey, pageURL string) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("capmonster api key empty")
	}
	created, err := c.post(ctx, "createTask", map[string]any{
		"clientKey": c.apiKey,
		"task": map[string]any{
			"type":       "TurnstileTaskProxyless",
			"websiteURL": pageURL,
			"websiteKey": sitekey,
		},
	})
	if err != nil {
		return "", err
	}
	if intValue(created["errorId"]) != 0 {
		return "", fmt.Errorf("capmonster createTask: %v", created["errorDescription"])
	}
	taskID := created["taskId"]
	if taskID == nil {
		return "", fmt.Errorf("capmonster missing taskId")
	}
	deadline := time.Now().Add(c.timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
		result, err := c.post(ctx, "getTaskResult", map[string]any{
			"clientKey": c.apiKey,
			"taskId":    taskID,
		})
		if err != nil {
			return "", err
		}
		if intValue(result["errorId"]) != 0 {
			return "", fmt.Errorf("capmonster getTaskResult: %v", result["errorDescription"])
		}
		status, _ := result["status"].(string)
		if status == "ready" {
			solution, _ := result["solution"].(map[string]any)
			token, _ := solution["token"].(string)
			if strings.TrimSpace(token) == "" {
				return "", fmt.Errorf("capmonster missing token")
			}
			return token, nil
		}
	}
	return "", fmt.Errorf("capmonster turnstile solve timeout")
}

func (c *CapMonster) post(ctx context.Context, endpoint string, body map[string]any) (map[string]any, error) {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/"+endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("capmonster %s HTTP %d: %s", endpoint, resp.StatusCode, truncate(string(raw), 200))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

type Auto struct {
	local      *Local
	capmonster *CapMonster
}

func NewAuto(local *Local, capmonster *CapMonster) *Auto {
	return &Auto{local: local, capmonster: capmonster}
}

func (a *Auto) Name() string { return "auto" }

func (a *Auto) Healthy(ctx context.Context) error {
	if a.local != nil {
		if err := a.local.Healthy(ctx); err == nil {
			return nil
		}
	}
	if a.capmonster != nil {
		return a.capmonster.Healthy(ctx)
	}
	return fmt.Errorf("no turnstile solver healthy")
}

func (a *Auto) Solve(ctx context.Context, sitekey, pageURL string) (string, error) {
	var errs []string
	if a.local != nil {
		token, err := a.local.Solve(ctx, sitekey, pageURL)
		if err == nil {
			return token, nil
		}
		errs = append(errs, "local: "+err.Error())
	}
	if a.capmonster != nil {
		token, err := a.capmonster.Solve(ctx, sitekey, pageURL)
		if err == nil {
			return token, nil
		}
		errs = append(errs, "capmonster: "+err.Error())
	}
	if len(errs) == 0 {
		return "", fmt.Errorf("no turnstile solver configured")
	}
	return "", fmt.Errorf("turnstile failed: %s", strings.Join(errs, " | "))
}

func NewFromMode(mode, localURL, capBase, capKey string, timeout time.Duration, client *http.Client) (Solver, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	local := NewLocal(localURL, timeout, client)
	cap := NewCapMonster(capBase, capKey, timeout, client)
	switch mode {
	case "local":
		return local, nil
	case "capmonster":
		if strings.TrimSpace(capKey) == "" {
			return nil, fmt.Errorf("capmonster api key required")
		}
		return cap, nil
	case "", "auto":
		return NewAuto(local, cap), nil
	default:
		return nil, fmt.Errorf("unsupported turnstile solver %q", mode)
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	default:
		return 0
	}
}

func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
