package mail

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/internal/config"
)

func NewProvider(settings config.Config, httpClient *http.Client) (Provider, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	switch strings.ToLower(strings.TrimSpace(settings.EmailProvider)) {
	case "", "cfmail":
		provider := NewCfmailProvider(settings.CfmailAccounts, settings.CfmailProfile, httpClient)
		if !provider.HasAccounts() {
			return nil, fmt.Errorf("no usable cfmail accounts configured")
		}
		return provider, nil
	case "mailtm":
		return NewMailtmProvider(settings.MailtmAPIBase, settings.MailtmDomain, httpClient), nil
	default:
		return nil, fmt.Errorf("unsupported email provider %q", settings.EmailProvider)
	}
}
