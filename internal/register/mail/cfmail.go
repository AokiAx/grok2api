package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/config"
)

type CfmailAccount struct {
	Name          string
	WorkerDomain  string
	EmailDomain   string
	AdminPassword string
}

type CfmailProvider struct {
	accounts        []CfmailAccount
	profileMode     string
	client          *http.Client
	mu              sync.Mutex
	index           int
	failThreshold   int
	cooldownSeconds int
	failures        map[string]*cfmailFailure
	activeName      string
}

type cfmailFailure struct {
	consecutive   int
	cooldownUntil time.Time
	lastError     string
}

func NewCfmailProvider(accounts []config.CfmailAccount, profileMode string, httpClient *http.Client) *CfmailProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	clean := make([]CfmailAccount, 0, len(accounts))
	for _, item := range accounts {
		if item.Enabled != nil && !*item.Enabled {
			continue
		}
		account := CfmailAccount{
			Name:          strings.TrimSpace(item.Name),
			WorkerDomain:  NormalizeHost(item.WorkerDomain),
			EmailDomain:   NormalizeHost(item.EmailDomain),
			AdminPassword: strings.TrimSpace(item.AdminPassword),
		}
		if account.Name == "" || account.WorkerDomain == "" || account.EmailDomain == "" || account.AdminPassword == "" {
			continue
		}
		clean = append(clean, account)
	}
	return &CfmailProvider{
		accounts:        clean,
		profileMode:     strings.TrimSpace(profileMode),
		client:          httpClient,
		failThreshold:   3,
		cooldownSeconds: 1800,
		failures:        map[string]*cfmailFailure{},
	}
}

func (p *CfmailProvider) Name() string { return "cfmail" }

func (p *CfmailProvider) HasAccounts() bool { return len(p.accounts) > 0 }

func (p *CfmailProvider) CreateMailbox(ctx context.Context) (string, string, error) {
	account, err := p.pickAccount()
	if err != nil {
		return "", "", err
	}
	p.activeName = account.Name
	local := "tmpgk" + strings.ToLower(randomFromAlphabet("abcdefghijklmnopqrstuvwxyz0123456789", 12))
	payload := map[string]any{
		"enablePrefix": true,
		"name":         local,
		"domain":       account.EmailDomain,
	}
	body, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("https://%s/admin/new_address", account.WorkerDomain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-admin-auth", account.AdminPassword)
	resp, err := p.client.Do(req)
	if err != nil {
		p.RecordFailure(err.Error())
		return "", "", fmt.Errorf("cfmail new_address: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		p.RecordFailure(string(raw))
		return "", "", fmt.Errorf("cfmail new_address HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var decoded struct {
		Address string `json:"address"`
		JWT     string `json:"jwt"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		p.RecordFailure(err.Error())
		return "", "", fmt.Errorf("cfmail decode: %w", err)
	}
	email := strings.TrimSpace(decoded.Address)
	token := strings.TrimSpace(firstNonEmpty(decoded.JWT, decoded.Token))
	if email == "" || token == "" {
		p.RecordFailure("missing address/jwt")
		return "", "", fmt.Errorf("cfmail response missing address/jwt")
	}
	return email, token, nil
}

func (p *CfmailProvider) WaitCode(ctx context.Context, token string, email string) (string, error) {
	account, err := p.accountByName(p.activeName)
	if err != nil {
		// fallback first account
		if len(p.accounts) == 0 {
			return "", err
		}
		account = p.accounts[0]
	}
	deadline := time.Now().Add(2 * time.Minute)
	if timeout, ok := ctx.Deadline(); ok {
		deadline = timeout
	}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		code, err := p.pollOnce(ctx, account, token)
		if err == nil && code != "" {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("cfmail code timeout for %s", email)
}

func (p *CfmailProvider) pollOnce(ctx context.Context, account CfmailAccount, token string) (string, error) {
	endpoint := fmt.Sprintf("https://%s/api/mails?limit=20&offset=0", account.WorkerDomain)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("cfmail mails HTTP %d", resp.StatusCode)
	}
	// Accept either {results:[{...}]} or array.
	var asObject struct {
		Results []map[string]any `json:"results"`
		Mails   []map[string]any `json:"mails"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		for _, item := range append(asObject.Results, asObject.Mails...) {
			if code := extractFromMailMap(item); code != "" {
				return code, nil
			}
		}
	}
	var asArray []map[string]any
	if err := json.Unmarshal(raw, &asArray); err == nil {
		for _, item := range asArray {
			if code := extractFromMailMap(item); code != "" {
				return code, nil
			}
		}
	}
	return "", fmt.Errorf("no code yet")
}

func extractFromMailMap(item map[string]any) string {
	parts := []string{}
	for _, key := range []string{"raw", "text", "html", "subject", "source"} {
		if value, ok := item[key].(string); ok {
			parts = append(parts, value)
		}
	}
	return ExtractOTPCode(strings.Join(parts, "\n"))
}

func (p *CfmailProvider) RecordSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := strings.ToLower(p.activeName)
	if key == "" {
		return
	}
	p.failures[key] = &cfmailFailure{}
}

func (p *CfmailProvider) RecordFailure(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := strings.ToLower(p.activeName)
	if key == "" && len(p.accounts) > 0 {
		key = strings.ToLower(p.accounts[0].Name)
	}
	if key == "" {
		return
	}
	state := p.failures[key]
	if state == nil {
		state = &cfmailFailure{}
		p.failures[key] = state
	}
	state.consecutive++
	state.lastError = truncate(reason, 200)
	if state.consecutive >= p.failThreshold {
		state.cooldownUntil = time.Now().Add(time.Duration(p.cooldownSeconds) * time.Second)
	}
}

func (p *CfmailProvider) pickAccount() (CfmailAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accounts) == 0 {
		return CfmailAccount{}, fmt.Errorf("no cfmail accounts configured")
	}
	now := time.Now()
	for i := 0; i < len(p.accounts); i++ {
		idx := (p.index + i) % len(p.accounts)
		account := p.accounts[idx]
		key := strings.ToLower(account.Name)
		if state := p.failures[key]; state != nil && state.cooldownUntil.After(now) {
			continue
		}
		p.index = idx + 1
		return account, nil
	}
	// all cooling down
	return CfmailAccount{}, fmt.Errorf("all cfmail profiles cooling down")
}

func (p *CfmailProvider) accountByName(name string) (CfmailAccount, error) {
	for _, account := range p.accounts {
		if strings.EqualFold(account.Name, name) {
			return account, nil
		}
	}
	return CfmailAccount{}, fmt.Errorf("cfmail account %q not found", name)
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
