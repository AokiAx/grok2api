package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/AokiAx/grok2api/internal/account"
)

type Client struct {
	baseURL       string
	clientVersion string
	httpClient    *http.Client
}

func NewClient(baseURL, clientVersion string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		clientVersion: clientVersion,
		httpClient:    httpClient,
	}
}

func (c *Client) Chat(
	ctx context.Context,
	item account.Account,
	payload []byte,
	stream bool,
) (*http.Response, error) {
	model := ""
	var requestBody struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(payload, &requestBody)
	model = requestBody.Model

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/chat/completions",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("create upstream chat request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+item.AccessToken)
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", c.clientVersion)
	request.Header.Set("x-grok-model-override", model)
	request.Header.Set("User-Agent", "xai-grok-build/"+c.clientVersion)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	} else {
		request.Header.Set("Accept", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send upstream chat request: %w", err)
	}
	return response, nil
}

func (c *Client) Validate(
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
	request.Header.Set("Authorization", "Bearer "+item.AccessToken)
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", c.clientVersion)
	request.Header.Set("User-Agent", "xai-grok-build/"+c.clientVersion)
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
