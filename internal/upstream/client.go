package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
