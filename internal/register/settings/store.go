package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AokiAx/grok2api/internal/config"
)

const fileName = "register_settings.json"

// Store persists register-related settings under data_dir.
type Store struct {
	mu   sync.RWMutex
	path string
	cur  config.Config
}

func NewStore(dataDir string, seed config.Config) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	store := &Store{
		path: filepath.Join(dataDir, fileName),
		cur:  seedRegister(seed),
	}
	if err := store.loadOrSeed(); err != nil {
		return nil, err
	}
	return store, nil
}

func seedRegister(seed config.Config) config.Config {
	out := config.Defaults()
	out.AccountsBase = first(seed.AccountsBase, out.AccountsBase)
	out.TurnstileSitekey = first(seed.TurnstileSitekey, out.TurnstileSitekey)
	out.CapMonsterAPIBase = first(seed.CapMonsterAPIBase, out.CapMonsterAPIBase)
	out.CapMonsterAPIKey = seed.CapMonsterAPIKey
	out.TurnstileSolver = first(seed.TurnstileSolver, out.TurnstileSolver)
	out.TurnstileSolverURL = first(seed.TurnstileSolverURL, out.TurnstileSolverURL)
	out.TurnstileTimeoutSec = nonzero(seed.TurnstileTimeoutSec, out.TurnstileTimeoutSec)
	out.EmailCodeTimeoutSec = nonzero(seed.EmailCodeTimeoutSec, out.EmailCodeTimeoutSec)
	out.Proxy = seed.Proxy
	out.ProxyPool = append([]string(nil), seed.ProxyPool...)
	out.ProxyRotate = first(seed.ProxyRotate, out.ProxyRotate)
	out.ImpersonateBrowser = first(seed.ImpersonateBrowser, out.ImpersonateBrowser)
	out.TokenJSONDir = first(seed.TokenJSONDir, out.TokenJSONDir)
	out.EmailProvider = first(seed.EmailProvider, out.EmailProvider)
	out.CfmailProfile = first(seed.CfmailProfile, out.CfmailProfile)
	out.CfmailAccounts = append([]config.CfmailAccount(nil), seed.CfmailAccounts...)
	out.MailtmAPIBase = first(seed.MailtmAPIBase, out.MailtmAPIBase)
	out.MailtmDomain = seed.MailtmDomain
	out.TotalAccounts = nonzero(seed.TotalAccounts, out.TotalAccounts)
	out.MaxWorkers = nonzero(seed.MaxWorkers, out.MaxWorkers)
	out.FlareSolverrURL = seed.FlareSolverrURL
	out.FlareSolverrEnabled = seed.FlareSolverrEnabled
	out.RegisterBackupTokens = seed.RegisterBackupTokens
	return normalizeRegister(out)
}

func (s *Store) loadOrSeed() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s.persistLocked()
		}
		return fmt.Errorf("read register settings: %w", err)
	}
	var loaded config.Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parse register settings: %w", err)
	}
	s.cur = seedRegister(loaded)
	return nil
}

func (s *Store) Get() config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cur)
}

func (s *Store) Update(patch config.Config) (config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cur
	next.AccountsBase = prefer(patch.AccountsBase, next.AccountsBase)
	next.TurnstileSitekey = prefer(patch.TurnstileSitekey, next.TurnstileSitekey)
	next.CapMonsterAPIBase = prefer(patch.CapMonsterAPIBase, next.CapMonsterAPIBase)
	// Empty capmonster key keeps previous; send single space to clear.
	if strings.TrimSpace(patch.CapMonsterAPIKey) == " " {
		next.CapMonsterAPIKey = ""
	} else if strings.TrimSpace(patch.CapMonsterAPIKey) != "" {
		next.CapMonsterAPIKey = strings.TrimSpace(patch.CapMonsterAPIKey)
	}
	next.TurnstileSolver = prefer(patch.TurnstileSolver, next.TurnstileSolver)
	next.TurnstileSolverURL = prefer(patch.TurnstileSolverURL, next.TurnstileSolverURL)
	if patch.TurnstileTimeoutSec > 0 {
		next.TurnstileTimeoutSec = patch.TurnstileTimeoutSec
	}
	if patch.EmailCodeTimeoutSec > 0 {
		next.EmailCodeTimeoutSec = patch.EmailCodeTimeoutSec
	}
	next.Proxy = strings.TrimSpace(patch.Proxy)
	if patch.ProxyPool != nil {
		next.ProxyPool = append([]string(nil), patch.ProxyPool...)
	}
	next.ProxyRotate = prefer(patch.ProxyRotate, next.ProxyRotate)
	next.ImpersonateBrowser = prefer(patch.ImpersonateBrowser, next.ImpersonateBrowser)
	next.TokenJSONDir = prefer(patch.TokenJSONDir, next.TokenJSONDir)
	next.EmailProvider = prefer(patch.EmailProvider, next.EmailProvider)
	next.CfmailProfile = prefer(patch.CfmailProfile, next.CfmailProfile)
	if patch.CfmailAccounts != nil {
		next.CfmailAccounts = append([]config.CfmailAccount(nil), patch.CfmailAccounts...)
	}
	next.MailtmAPIBase = prefer(patch.MailtmAPIBase, next.MailtmAPIBase)
	next.MailtmDomain = strings.TrimSpace(patch.MailtmDomain)
	if patch.TotalAccounts > 0 {
		next.TotalAccounts = patch.TotalAccounts
	}
	if patch.MaxWorkers > 0 {
		next.MaxWorkers = patch.MaxWorkers
	}
	next.FlareSolverrURL = strings.TrimSpace(patch.FlareSolverrURL)
	next.FlareSolverrEnabled = patch.FlareSolverrEnabled
	next.RegisterBackupTokens = patch.RegisterBackupTokens

	next = normalizeRegister(next)
	s.cur = next
	if err := s.persistLocked(); err != nil {
		return config.Config{}, err
	}
	return cloneConfig(s.cur), nil
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.cur, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write register settings: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace register settings: %w", err)
	}
	return nil
}

func PublicView(cfg config.Config) map[string]any {
	return map[string]any{
		"accounts_base":          cfg.AccountsBase,
		"turnstile_sitekey":      cfg.TurnstileSitekey,
		"capmonster_api_base":    cfg.CapMonsterAPIBase,
		"capmonster_api_key_set": strings.TrimSpace(cfg.CapMonsterAPIKey) != "",
		"turnstile_solver":       cfg.TurnstileSolver,
		"turnstile_solver_url":   cfg.TurnstileSolverURL,
		"turnstile_timeout":      cfg.TurnstileTimeoutSec,
		"email_code_timeout":     cfg.EmailCodeTimeoutSec,
		"proxy":                  cfg.Proxy,
		"proxy_pool":             cfg.ProxyPool,
		"proxy_rotate":           cfg.ProxyRotate,
		"impersonate_browser":    cfg.ImpersonateBrowser,
		"email_provider":         cfg.EmailProvider,
		"cfmail_profile":         cfg.CfmailProfile,
		"cfmail_accounts":        publicCfmail(cfg.CfmailAccounts),
		"mailtm_api_base":        cfg.MailtmAPIBase,
		"mailtm_domain":          cfg.MailtmDomain,
		"total_accounts":         cfg.TotalAccounts,
		"max_workers":            cfg.MaxWorkers,
		"flaresolverr_url":       cfg.FlareSolverrURL,
		"flaresolverr_enabled":   cfg.FlareSolverrEnabled,
		"register_backup_tokens": cfg.RegisterBackupTokens,
	}
}

func EditorView(cfg config.Config) map[string]any {
	accounts := make([]map[string]any, 0, len(cfg.CfmailAccounts))
	for _, item := range cfg.CfmailAccounts {
		enabled := true
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		accounts = append(accounts, map[string]any{
			"name":           item.Name,
			"worker_domain":  item.WorkerDomain,
			"email_domain":   item.EmailDomain,
			"admin_password": item.AdminPassword,
			"enabled":        enabled,
		})
	}
	return map[string]any{
		"accounts_base":          cfg.AccountsBase,
		"turnstile_sitekey":      cfg.TurnstileSitekey,
		"capmonster_api_base":    cfg.CapMonsterAPIBase,
		"capmonster_api_key":     cfg.CapMonsterAPIKey,
		"turnstile_solver":       cfg.TurnstileSolver,
		"turnstile_solver_url":   cfg.TurnstileSolverURL,
		"turnstile_timeout":      cfg.TurnstileTimeoutSec,
		"email_code_timeout":     cfg.EmailCodeTimeoutSec,
		"proxy":                  cfg.Proxy,
		"proxy_pool":             cfg.ProxyPool,
		"proxy_rotate":           cfg.ProxyRotate,
		"impersonate_browser":    cfg.ImpersonateBrowser,
		"email_provider":         cfg.EmailProvider,
		"cfmail_profile":         cfg.CfmailProfile,
		"cfmail_accounts":        accounts,
		"mailtm_api_base":        cfg.MailtmAPIBase,
		"mailtm_domain":          cfg.MailtmDomain,
		"total_accounts":         cfg.TotalAccounts,
		"max_workers":            cfg.MaxWorkers,
		"flaresolverr_url":       cfg.FlareSolverrURL,
		"flaresolverr_enabled":   cfg.FlareSolverrEnabled,
		"register_backup_tokens": cfg.RegisterBackupTokens,
	}
}

func publicCfmail(items []config.CfmailAccount) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		enabled := true
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		out = append(out, map[string]any{
			"name":               item.Name,
			"worker_domain":      item.WorkerDomain,
			"email_domain":       item.EmailDomain,
			"admin_password_set": strings.TrimSpace(item.AdminPassword) != "",
			"enabled":            enabled,
		})
	}
	return out
}

func normalizeRegister(in config.Config) config.Config {
	out := in
	if strings.TrimSpace(out.AccountsBase) == "" {
		out.AccountsBase = config.Defaults().AccountsBase
	}
	out.AccountsBase = strings.TrimRight(strings.TrimSpace(out.AccountsBase), "/")
	if strings.TrimSpace(out.TurnstileSolver) == "" {
		out.TurnstileSolver = "auto"
	}
	out.TurnstileSolver = strings.ToLower(strings.TrimSpace(out.TurnstileSolver))
	if out.TurnstileTimeoutSec <= 0 {
		out.TurnstileTimeoutSec = 120
	}
	if out.EmailCodeTimeoutSec <= 0 {
		out.EmailCodeTimeoutSec = 120
	}
	if out.TotalAccounts <= 0 {
		out.TotalAccounts = 1
	}
	if out.MaxWorkers <= 0 {
		out.MaxWorkers = 1
	}
	if strings.TrimSpace(out.EmailProvider) == "" {
		out.EmailProvider = "cfmail"
	}
	out.EmailProvider = strings.ToLower(strings.TrimSpace(out.EmailProvider))
	if strings.TrimSpace(out.ProxyRotate) == "" {
		out.ProxyRotate = "per_account"
	}
	if strings.TrimSpace(out.CapMonsterAPIBase) == "" {
		out.CapMonsterAPIBase = config.Defaults().CapMonsterAPIBase
	}
	out.CapMonsterAPIBase = strings.TrimRight(strings.TrimSpace(out.CapMonsterAPIBase), "/")
	if strings.TrimSpace(out.MailtmAPIBase) == "" {
		out.MailtmAPIBase = config.Defaults().MailtmAPIBase
	}
	out.MailtmAPIBase = strings.TrimRight(strings.TrimSpace(out.MailtmAPIBase), "/")
	out.TurnstileSolverURL = strings.TrimRight(strings.TrimSpace(out.TurnstileSolverURL), "/")
	out.FlareSolverrURL = strings.TrimRight(strings.TrimSpace(out.FlareSolverrURL), "/")
	return out
}

func cloneConfig(in config.Config) config.Config {
	out := in
	out.ProxyPool = append([]string(nil), in.ProxyPool...)
	out.CfmailAccounts = append([]config.CfmailAccount(nil), in.CfmailAccounts...)
	return out
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func prefer(patch, current string) string {
	if strings.TrimSpace(patch) != "" {
		return strings.TrimSpace(patch)
	}
	return current
}

func nonzero(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
