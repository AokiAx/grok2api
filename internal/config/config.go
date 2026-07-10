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
	QuotaRetryMinutes int    `json:"quota_retry_minutes"`
	RateRetrySeconds  int    `json:"rate_retry_seconds"`
	RequestTimeoutSec int    `json:"timeout_secs"`
}

func Defaults() Config {
	return Config{
		Host:              "127.0.0.1",
		Port:              8787,
		ProxyBaseURL:      "https://cli-chat-proxy.grok.com/v1",
		ClientVersion:     "0.2.93",
		DefaultModel:      "grok-4.5",
		DataDir:           "data",
		MaxConcurrent:     1,
		AcquireTimeoutSec: 60,
		QuotaRetryMinutes: 30,
		RateRetrySeconds:  45,
		RequestTimeoutSec: 600,
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
	return config, nil
}

func applyEnvironment(config *Config) error {
	stringValues := map[string]*string{
		"GROK2API_HOST":           &config.Host,
		"GROK2API_API_KEY":        &config.APIKey,
		"GROK2API_APP_KEY":        &config.AppKey,
		"GROK2API_PANEL_PASSWORD": &config.PanelPassword,
		"GROK2API_PROXY_BASE_URL": &config.ProxyBaseURL,
		"GROK2API_CLIENT_VERSION": &config.ClientVersion,
		"GROK2API_DEFAULT_MODEL":  &config.DefaultModel,
		"GROK2API_DATA_DIR":       &config.DataDir,
	}
	for name, target := range stringValues {
		if value, ok := os.LookupEnv(name); ok {
			*target = strings.TrimSpace(value)
		}
	}

	integerValues := map[string]*int{
		"GROK2API_PORT":                     &config.Port,
		"GROK2API_CLI_POOL_MAX_CONCURRENT":  &config.MaxConcurrent,
		"GROK2API_CLI_POOL_ACQUIRE_TIMEOUT": &config.AcquireTimeoutSec,
		"GROK2API_QUOTA_RETRY_MINUTES":      &config.QuotaRetryMinutes,
		"GROK2API_RATE_RETRY_SECONDS":       &config.RateRetrySeconds,
		"GROK2API_TIMEOUT_SECS":             &config.RequestTimeoutSec,
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
