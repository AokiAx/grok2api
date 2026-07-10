package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type MailtmProvider struct {
	apiBase string
	domain  string
	client  *http.Client
}

func NewMailtmProvider(apiBase, preferredDomain string, httpClient *http.Client) *MailtmProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		apiBase = "https://api.mail.tm"
	}
	return &MailtmProvider{
		apiBase: apiBase,
		domain:  strings.Trim(strings.ToLower(strings.TrimSpace(preferredDomain)), "."),
		client:  httpClient,
	}
}

func (p *MailtmProvider) Name() string { return "mailtm" }

func (p *MailtmProvider) HasAccounts() bool { return true }

func (p *MailtmProvider) CreateMailbox(ctx context.Context) (string, string, error) {
	domain := p.domain
	if domain == "" {
		var err error
		domain, err = p.firstDomain(ctx)
		if err != nil {
			return "", "", err
		}
	}
	local := "tmpgk" + strings.ToLower(randomFromAlphabet("abcdefghijklmnopqrstuvwxyz0123456789", 10))
	address := local + "@" + domain
	password := GeneratePassword(16)
	createBody, _ := json.Marshal(map[string]string{
		"address":  address,
		"password": password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/accounts", bytes.NewReader(createBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("mailtm create account: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("mailtm create account HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	tokenBody, _ := json.Marshal(map[string]string{
		"address":  address,
		"password": password,
	})
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/token", bytes.NewReader(tokenBody))
	if err != nil {
		return "", "", err
	}
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenResp, err := p.client.Do(tokenReq)
	if err != nil {
		return "", "", fmt.Errorf("mailtm token: %w", err)
	}
	defer tokenResp.Body.Close()
	tokenRaw, _ := io.ReadAll(io.LimitReader(tokenResp.Body, 1<<20))
	if tokenResp.StatusCode >= 400 {
		return "", "", fmt.Errorf("mailtm token HTTP %d: %s", tokenResp.StatusCode, truncate(string(tokenRaw), 200))
	}
	var tokenPayload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(tokenRaw, &tokenPayload); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(tokenPayload.Token) == "" {
		return "", "", fmt.Errorf("mailtm token missing")
	}
	return address, tokenPayload.Token, nil
}

func (p *MailtmProvider) WaitCode(ctx context.Context, token string, email string) (string, error) {
	deadline := time.Now().Add(2 * time.Minute)
	if timeout, ok := ctx.Deadline(); ok {
		deadline = timeout
	}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		code, err := p.pollOnce(ctx, token)
		if err == nil && code != "" {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("mailtm code timeout for %s", email)
}

func (p *MailtmProvider) pollOnce(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/messages", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("mailtm messages HTTP %d", resp.StatusCode)
	}
	var payload struct {
		HydraMember []map[string]any `json:"hydra:member"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	for _, item := range payload.HydraMember {
		id, _ := item["id"].(string)
		if id == "" {
			continue
		}
		detail, err := p.message(ctx, token, id)
		if err != nil {
			continue
		}
		if code := ExtractOTPCode(detail); code != "" {
			return code, nil
		}
	}
	return "", fmt.Errorf("no code yet")
}

func (p *MailtmProvider) message(ctx context.Context, token, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/messages/"+id, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("mailtm message HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return string(raw), nil
	}
	parts := []string{}
	for _, key := range []string{"text", "html", "subject", "intro"} {
		if value, ok := payload[key].(string); ok {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func (p *MailtmProvider) firstDomain(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/domains", nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("mailtm domains HTTP %d", resp.StatusCode)
	}
	var payload struct {
		HydraMember []struct {
			Domain string `json:"domain"`
		} `json:"hydra:member"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	if len(payload.HydraMember) == 0 || payload.HydraMember[0].Domain == "" {
		return "", fmt.Errorf("mailtm domains empty")
	}
	return payload.HydraMember[0].Domain, nil
}

func (p *MailtmProvider) RecordSuccess()            {}
func (p *MailtmProvider) RecordFailure(reason string) {}
