package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds API server and pool settings only.
// Account registration lives in the external grok-register project.
type Config struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	APIKey             string `json:"api_key"`
	AppKey             string `json:"app_key"`
	PanelPassword      string `json:"panel_password"`
	AdminSecureCookies bool   `json:"admin_secure_cookies"`
	ProxyBaseURL       string `json:"proxy_base_url"`
	ClientVersion      string `json:"client_version"`
	// CLI fingerprint for the Grok CLI request surface.
	ClientIdentifier  string `json:"client_identifier"`
	ClientUserAgent   string `json:"client_user_agent"`
	TokenAuth         string `json:"token_auth"`
	CredentialKey     string `json:"credential_key"`
	DefaultModel      string `json:"default_model"`
	DataDir           string `json:"data_dir"`
	MaxConcurrent     int    `json:"cli_pool_max_concurrent"`
	AcquireTimeoutSec int    `json:"cli_pool_acquire_timeout"`
	// MaxAttempts caps how many ready accounts a single request may burn when
	// rotating through quota/auth/permission-denied failures.
	MaxAttempts int `json:"cli_pool_max_attempts"`
	// Strategy is round-robin (default) or fill-first.
	Strategy string `json:"cli_pool_strategy"`
	// ActiveSize optionally caps hot-set size; 0 = full ready pool.
	ActiveSize int `json:"cli_pool_active_size"`
	// Sticky pool keeps the same Grok account for a client session / prompt
	// fingerprint so prefix cache (cached_tokens) stays warm.
	StickyPool        bool `json:"cli_pool_sticky"`
	StickyTTLMinutes  int  `json:"cli_pool_sticky_ttl_minutes"`
	QuotaRetryMinutes int  `json:"quota_retry_minutes"`
	RateRetrySeconds  int  `json:"rate_retry_seconds"`
	RequestTimeoutSec int  `json:"timeout_secs"`

	// Optional HTTP proxy for outbound (e.g. privoxy). Not required for pool.
	Proxy string `json:"proxy"`

	// Temporary request interceptor for protocol debugging.
	DebugTrace           bool   `json:"debug_trace"`
	DebugTraceDir        string `json:"debug_trace_dir"`
	DebugTraceErrorsOnly bool   `json:"debug_trace_errors_only"`

	Frontend FrontendConfig `json:"frontend"`
}

type FrontendConfig struct {
	StaticPath string `json:"static_path"`
}

func Defaults() Config {
	return Config{
		Host:              "127.0.0.1",
		Port:              8787,
		ProxyBaseURL:      "https://cli-chat-proxy.grok.com/v1",
		ClientVersion:     "0.2.93",
		ClientIdentifier:  "grok-cli",
		TokenAuth:         "xai-grok-cli",
		DefaultModel:      "grok-4.5",
		DataDir:           "data",
		MaxConcurrent:     4,
		AcquireTimeoutSec: 60,
		MaxAttempts:       3,
		Strategy:          "round-robin",
		ActiveSize:        0,
		StickyPool:        true,
		StickyTTLMinutes:  30,
		QuotaRetryMinutes: 1440,
		RateRetrySeconds:  45,
		RequestTimeoutSec: 600,
		// Secure by default; set admin_secure_cookies=false for plain HTTP loopback panels.
		AdminSecureCookies: true,
	}
}

func Load(path string) (Config, error) {
	config := Defaults()
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := applyEnvironment(&config); err != nil {
		return Config{}, err
	}
	normalize(&config)
	return config, nil
}

func normalize(config *Config) {
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	switch strings.ToLower(strings.TrimSpace(config.Strategy)) {
	case "fill-first", "fill_first", "fillfirst":
		config.Strategy = "fill-first"
	default:
		config.Strategy = "round-robin"
	}
	if config.ActiveSize < 0 {
		config.ActiveSize = 0
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 1
	}
	if strings.TrimSpace(config.ProxyBaseURL) == "" {
		config.ProxyBaseURL = Defaults().ProxyBaseURL
	}
	config.ProxyBaseURL = strings.TrimRight(strings.TrimSpace(config.ProxyBaseURL), "/")
	if strings.TrimSpace(config.ClientIdentifier) == "" {
		config.ClientIdentifier = Defaults().ClientIdentifier
	}
	if strings.TrimSpace(config.TokenAuth) == "" {
		config.TokenAuth = Defaults().TokenAuth
	}
	if strings.TrimSpace(config.DefaultModel) == "" {
		config.DefaultModel = Defaults().DefaultModel
	}
	if strings.TrimSpace(config.DataDir) == "" {
		config.DataDir = "data"
	}
	if config.Port <= 0 {
		config.Port = 8787
	}
	if config.RequestTimeoutSec <= 0 {
		config.RequestTimeoutSec = 600
	}
	if config.QuotaRetryMinutes <= 0 {
		config.QuotaRetryMinutes = 1440
	}
	if config.RateRetrySeconds <= 0 {
		config.RateRetrySeconds = 45
	}
	if config.StickyTTLMinutes <= 0 {
		config.StickyTTLMinutes = 30
	}
	if config.AcquireTimeoutSec <= 0 {
		config.AcquireTimeoutSec = 60
	}
}

func applyEnvironment(config *Config) error {
	stringValues := map[string]*string{
		"GROK2API_HOST":                 &config.Host,
		"GROK2API_API_KEY":              &config.APIKey,
		"GROK2API_APP_KEY":              &config.AppKey,
		"GROK2API_PANEL_PASSWORD":       &config.PanelPassword,
		"GROK2API_PROXY_BASE_URL":       &config.ProxyBaseURL,
		"GROK2API_CLIENT_VERSION":       &config.ClientVersion,
		"GROK2API_CLIENT_IDENTIFIER":    &config.ClientIdentifier,
		"GROK2API_CLIENT_USER_AGENT":    &config.ClientUserAgent,
		"GROK2API_TOKEN_AUTH":           &config.TokenAuth,
		"GROK2API_CREDENTIAL_KEY":       &config.CredentialKey,
		"GROK2API_DEFAULT_MODEL":        &config.DefaultModel,
		"GROK2API_DATA_DIR":             &config.DataDir,
		"GROK2API_PROXY":                &config.Proxy,
		"PROXY_URL":                     &config.Proxy,
		"GROK2API_DEBUG_TRACE_DIR":      &config.DebugTraceDir,
		"GROK2API_FRONTEND_STATIC_PATH": &config.Frontend.StaticPath,
	}
	for name, target := range stringValues {
		if value, ok := os.LookupEnv(name); ok {
			*target = strings.TrimSpace(value)
		}
	}

	integerValues := map[string]*int{
		"GROK2API_PORT":                        &config.Port,
		"GROK2API_CLI_POOL_MAX_CONCURRENT":     &config.MaxConcurrent,
		"GROK2API_CLI_POOL_ACQUIRE_TIMEOUT":    &config.AcquireTimeoutSec,
		"GROK2API_CLI_POOL_STICKY_TTL_MINUTES": &config.StickyTTLMinutes,
		"GROK2API_QUOTA_RETRY_MINUTES":         &config.QuotaRetryMinutes,
		"GROK2API_RATE_RETRY_SECONDS":          &config.RateRetrySeconds,
		"GROK2API_TIMEOUT_SECS":                &config.RequestTimeoutSec,
	}
	for name, target := range integerValues {
		value, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		*target = parsed
	}

	boolValues := map[string]*bool{
		"GROK2API_ADMIN_SECURE_COOKIES":    &config.AdminSecureCookies,
		"GROK2API_DEBUG_TRACE":             &config.DebugTrace,
		"GROK2API_DEBUG_TRACE_ERRORS_ONLY": &config.DebugTraceErrorsOnly,
		"GROK2API_CLI_POOL_STICKY":         &config.StickyPool,
	}
	for name, target := range boolValues {
		value, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			*target = true
		case "0", "false", "no", "off":
			*target = false
		default:
			return fmt.Errorf("parse %s: invalid boolean %q", name, value)
		}
	}
	return nil
}

func (c Config) Address() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSec) * time.Second
}

func (c Config) AdminKey() string {
	for _, key := range []string{c.PanelPassword, c.AppKey} {
		if key = strings.TrimSpace(key); key != "" {
			return key
		}
	}
	return ""
}
