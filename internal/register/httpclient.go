package register

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// HTTPClient builds an HTTP client. When proxyURL is non-empty, all traffic
// (mail / signup / mint) uses the same egress path.
func HTTPClient(proxyURL string, timeout time.Duration, withJar bool) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	transport := http.DefaultTransport
	if raw := strings.TrimSpace(proxyURL); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	if withJar {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		client.Jar = jar
	}
	return client, nil
}
