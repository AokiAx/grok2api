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

type CfmailAccount struct {
	Name          string `json:"name"`
	WorkerDomain  string `json:"worker_domain"`
	EmailDomain   string `json:"email_domain"`
	AdminPassword string `json:"admin_password"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

type Config struct {
	Host              string `json:"host"`
	Port              int    `json:"port"`
	APIKey            string `json:"api_key"`
	AppKey            string `json:"app_key"`
	PanelPassword     string `json:"panel_password"`
	ProxyBaseURL      string `json:"proxy_base_url"`
	ClientVersion     string `json:"client_version"`
	DefaultModel      string `json:"default_model"`
	DataDir           string `json:"data_dir"`
	MaxConcurrent     int    `json:"cli_pool_max_concurrent"`
	AcquireTimeoutSec int    `json:"cli_pool_acquire_timeout"`
	// MaxAttempts caps how many ready accounts a single request may burn when
	// rotating through quota/auth/permission-denied failures. Without this,
	// one bad request can park the entire ready pool.
	MaxAttempts int `json:"cli_pool_max_attempts"`
	// Sticky pool keeps the same Grok account for a client session / prompt
	// fingerprint so prefix cache (cached_tokens) stays warm.
	StickyPool        bool `json:"cli_pool_sticky"`
	StickyTTLMinutes  int  `json:"cli_pool_sticky_ttl_minutes"`
	QuotaRetryMinutes int  `json:"quota_retry_minutes"`
	RateRetrySeconds  int  `json:"rate_retry_seconds"`
	RequestTimeoutSec int  `json:"timeout_secs"`

	// Register / anti-bot settings (compatible with Python config.json keys).
	AccountsBase         string          `json:"accounts_base"`
	TurnstileSitekey     string          `json:"turnstile_sitekey"`
	CapMonsterAPIBase    string          `json:"capmonster_api_base"`
	CapMonsterAPIKey     string          `json:"capmonster_api_key"`
	TurnstileSolver      string          `json:"turnstile_solver"`
	TurnstileSolverURL   string          `json:"turnstile_solver_url"`
	TurnstileTimeoutSec  int             `json:"turnstile_timeout"`
	EmailCodeTimeoutSec  int             `json:"email_code_timeout"`
	Proxy                string          `json:"proxy"`
	ProxyPool            []string        `json:"proxy_pool"`
	ProxyRotate          string          `json:"proxy_rotate"`
	ImpersonateBrowser   string          `json:"impersonate_browser"`
	TokenJSONDir         string          `json:"token_json_dir"`
	EmailProvider        string          `json:"email_provider"`
	CfmailProfile        string          `json:"cfmail_profile"`
	CfmailAccounts       []CfmailAccount `json:"cfmail_accounts"`
	MailtmAPIBase        string          `json:"mailtm_api_base"`
	MailtmDomain         string          `json:"mailtm_domain"`
	TotalAccounts        int             `json:"total_accounts"`
	MaxWorkers           int             `json:"max_workers"`
	FlareSolverrURL      string          `json:"flaresolverr_url"`
	FlareSolverrEnabled  bool            `json:"flaresolverr_enabled"`
	RegisterBackupTokens bool            `json:"register_backup_tokens"`

	// Temporary request interceptor for protocol debugging.
	// Enable via "debug_trace": true or GROK2API_DEBUG_TRACE=1.
	DebugTrace    bool   `json:"debug_trace"`
	DebugTraceDir string `json:"debug_trace_dir"`
	// When true with debug_trace, only persist traces for status>=400 / errors.
	DebugTraceErrorsOnly bool `json:"debug_trace_errors_only"`
}

func Defaults() Config {
	return Config{
		Host:                "127.0.0.1",
		Port:                8787,
		ProxyBaseURL:        "https://cli-chat-proxy.grok.com/v1",
		ClientVersion:       "0.2.93",
		DefaultModel:        "grok-4.5",
		DataDir:             "data",
		MaxConcurrent:       1,
		AcquireTimeoutSec:   60,
		MaxAttempts:         3,
		StickyPool:          true,
		StickyTTLMinutes:    30,
		QuotaRetryMinutes:   1440,
		RateRetrySeconds:    45,
		RequestTimeoutSec:   600,
		AccountsBase:        "https://accounts.x.ai",
		TurnstileSitekey:    "0x4AAAAAAAhr9JGVDZbrZOo0",
		CapMonsterAPIBase:   "https://api.capmonster.cloud",
		TurnstileSolver:     "auto",
		TurnstileSolverURL:  "http://127.0.0.1:5072",
		TurnstileTimeoutSec: 120,
		EmailCodeTimeoutSec: 120,
		ProxyRotate:         "per_account",
		ImpersonateBrowser:  "chrome136",
		TokenJSONDir:        "register/output/grok_tokens",
		EmailProvider:       "cfmail",
		CfmailProfile:       "auto",
		MailtmAPIBase:       "https://api.mail.tm",
		TotalAccounts:       1,
		MaxWorkers:          1,
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
	if strings.TrimSpace(config.AccountsBase) == "" {
		config.AccountsBase = Defaults().AccountsBase
	}
	config.AccountsBase = strings.TrimRight(strings.TrimSpace(config.AccountsBase), "/")
	if strings.TrimSpace(config.TurnstileSolver) == "" {
		config.TurnstileSolver = "auto"
	}
	config.TurnstileSolver = strings.ToLower(strings.TrimSpace(config.TurnstileSolver))
	if config.TurnstileTimeoutSec <= 0 {
		config.TurnstileTimeoutSec = 120
	}
	if config.EmailCodeTimeoutSec <= 0 {
		config.EmailCodeTimeoutSec = 120
	}
	if config.TotalAccounts <= 0 {
		config.TotalAccounts = 1
	}
	if config.MaxWorkers <= 0 {
		config.MaxWorkers = 1
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 1
	}
	if strings.TrimSpace(config.EmailProvider) == "" {
		config.EmailProvider = "cfmail"
	}
	config.EmailProvider = strings.ToLower(strings.TrimSpace(config.EmailProvider))
	if strings.TrimSpace(config.ProxyRotate) == "" {
		config.ProxyRotate = "per_account"
	}
	config.ProxyRotate = strings.ToLower(strings.TrimSpace(config.ProxyRotate))
	if strings.TrimSpace(config.CapMonsterAPIBase) == "" {
		config.CapMonsterAPIBase = Defaults().CapMonsterAPIBase
	}
	config.CapMonsterAPIBase = strings.TrimRight(strings.TrimSpace(config.CapMonsterAPIBase), "/")
	if strings.TrimSpace(config.MailtmAPIBase) == "" {
		config.MailtmAPIBase = Defaults().MailtmAPIBase
	}
	config.MailtmAPIBase = strings.TrimRight(strings.TrimSpace(config.MailtmAPIBase), "/")
	if strings.TrimSpace(config.TurnstileSolverURL) != "" {
		config.TurnstileSolverURL = strings.TrimRight(strings.TrimSpace(config.TurnstileSolverURL), "/")
	}
	if strings.TrimSpace(config.FlareSolverrURL) != "" {
		config.FlareSolverrURL = strings.TrimRight(strings.TrimSpace(config.FlareSolverrURL), "/")
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
		"GROK2API_DEFAULT_MODEL":        &config.DefaultModel,
		"GROK2API_DATA_DIR":             &config.DataDir,
		"GROK2API_ACCOUNTS_BASE":        &config.AccountsBase,
		"ACCOUNTS_BASE":                 &config.AccountsBase,
		"GROK2API_TURNSTILE_SITEKEY":    &config.TurnstileSitekey,
		"TURNSTILE_SITEKEY":             &config.TurnstileSitekey,
		"GROK2API_CAPMONSTER_API_BASE":  &config.CapMonsterAPIBase,
		"CAPMONSTER_API_BASE":           &config.CapMonsterAPIBase,
		"GROK2API_CAPMONSTER_API_KEY":   &config.CapMonsterAPIKey,
		"CAPMONSTER_API_KEY":            &config.CapMonsterAPIKey,
		"GROK2API_TURNSTILE_SOLVER":     &config.TurnstileSolver,
		"GROK_TURNSTILE_SOLVER":         &config.TurnstileSolver,
		"GROK2API_TURNSTILE_SOLVER_URL": &config.TurnstileSolverURL,
		"GROK_TURNSTILE_SOLVER_URL":     &config.TurnstileSolverURL,
		"GROK2API_PROXY":                &config.Proxy,
		"PROXY_URL":                     &config.Proxy,
		"GROK2API_IMPERSONATE_BROWSER":  &config.ImpersonateBrowser,
		"IMPERSONATE_BROWSER":           &config.ImpersonateBrowser,
		"GROK2API_TOKEN_JSON_DIR":       &config.TokenJSONDir,
		"GROK_TOKEN_DIR":                &config.TokenJSONDir,
		"GROK2API_EMAIL_PROVIDER":       &config.EmailProvider,
		"GROK_EMAIL_PROVIDER":           &config.EmailProvider,
		"GROK2API_CFMAIL_PROFILE":       &config.CfmailProfile,
		"GROK_CFMAIL_PROFILE":           &config.CfmailProfile,
		"GROK2API_MAILTM_API_BASE":      &config.MailtmAPIBase,
		"GROK_MAILTM_API_BASE":          &config.MailtmAPIBase,
		"GROK2API_MAILTM_DOMAIN":        &config.MailtmDomain,
		"GROK_MAILTM_DOMAIN":            &config.MailtmDomain,
		"GROK2API_PROXY_ROTATE":         &config.ProxyRotate,
		"GROK2API_FLARESOLVERR_URL":     &config.FlareSolverrURL,
		"GROK2API_DEBUG_TRACE_DIR":      &config.DebugTraceDir,
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
		"GROK2API_TURNSTILE_TIMEOUT":           &config.TurnstileTimeoutSec,
		"TURNSTILE_TIMEOUT":                    &config.TurnstileTimeoutSec,
		"GROK2API_EMAIL_CODE_TIMEOUT":          &config.EmailCodeTimeoutSec,
		"EMAIL_CODE_TIMEOUT":                   &config.EmailCodeTimeoutSec,
		"GROK2API_TOTAL_ACCOUNTS":              &config.TotalAccounts,
		"TOTAL_ACCOUNTS":                       &config.TotalAccounts,
		"GROK2API_MAX_WORKERS":                 &config.MaxWorkers,
		"MAX_WORKERS":                          &config.MaxWorkers,
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
		"GROK2API_FLARESOLVERR_ENABLED":    &config.FlareSolverrEnabled,
		"GROK2API_REGISTER_BACKUP_TOKENS":  &config.RegisterBackupTokens,
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
	for _, key := range []string{c.PanelPassword, c.AppKey, c.APIKey} {
		if key = strings.TrimSpace(key); key != "" {
			return key
		}
	}
	return ""
}

func (c Config) TurnstileTimeout() time.Duration {
	return time.Duration(c.TurnstileTimeoutSec) * time.Second
}

func (c Config) EmailCodeTimeout() time.Duration {
	return time.Duration(c.EmailCodeTimeoutSec) * time.Second
}
