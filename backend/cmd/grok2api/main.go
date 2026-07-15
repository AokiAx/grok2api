package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/admin"
	"github.com/AokiAx/grok2api/backend/internal/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/bootstrap"
	"github.com/AokiAx/grok2api/backend/internal/clientkeys"
	"github.com/AokiAx/grok2api/backend/internal/config"
	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/domain/settings"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/intercept"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	runtimeworker "github.com/AokiAx/grok2api/backend/internal/runtime"
	"github.com/AokiAx/grok2api/backend/internal/scheduler"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"github.com/AokiAx/grok2api/backend/internal/service"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
	"golang.org/x/crypto/bcrypt"
)

const version = "1.0.0-go"

type runtimeRepository interface {
	repository.AccountRepository
	repository.AdminAuthRepository
	repository.ClientKeyRepository
	repository.LegacySecurityBootstrapRepository
	repository.AuditRepository
	repository.ModelRegistryRepository
	repository.SettingsRepository
}

type serveCommand func(context.Context, config.Config, runtimeRepository) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runWithIO(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		slog.Error("grok2api stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	return runWithIOAndServe(ctx, arguments, os.Stdin, output, serve)
}

func runWithIO(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	return runWithIOAndServe(ctx, arguments, input, output, serve)
}

func runWithServe(ctx context.Context, arguments []string, output io.Writer, serveFn serveCommand) error {
	return runWithIOAndServe(ctx, arguments, os.Stdin, output, serveFn)
}

func runWithIOAndServe(
	ctx context.Context,
	arguments []string,
	input io.Reader,
	output io.Writer,
	serveFn serveCommand,
) error {
	command := "serve"
	if len(arguments) > 0 && arguments[0] != "--config" {
		command = arguments[0]
		arguments = arguments[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(output)
	configPath := flags.String("config", "config.json", "configuration file")
	exportPath := flags.String("out", "", "output file path for export command (default data/export_accounts.json)")
	exportPool := flags.String("pool", "", "export only accounts in this pool (ready|unavailable); empty = all")
	passwordStdin := flags.Bool("password-stdin", false, "read the administrator bootstrap password from stdin")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if command != "bootstrap-admin" && *passwordStdin {
		return errors.New("--password-stdin is only valid for bootstrap-admin")
	}

	settings, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(settings.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	credCipher, err := security.NewCipher(settings.CredentialKey)
	if err != nil {
		return fmt.Errorf("credential cipher: %w", err)
	}
	if credCipher != nil {
		slog.Info("credential encryption enabled")
	}
	repo, err := sqlite.OpenSQLiteWithCipher(ctx, filepath.Join(settings.DataDir, "grok2api.db"), credCipher)
	if err != nil {
		return err
	}
	defer repo.Close()
	if command != "bootstrap-admin" {
		if err := importLegacyWhenEmpty(ctx, repo, settings.DataDir); err != nil {
			return err
		}
	}

	switch command {
	case "bootstrap-admin":
		if !*passwordStdin {
			return errors.New("bootstrap-admin requires --password-stdin")
		}
		return runBootstrapAdmin(ctx, input, output, repo)
	case "migrate", "status":
		return printStatus(ctx, output, repo)
	case "serve":
		if serveFn == nil {
			return errors.New("serve command is required")
		}
		if err := bootstrapServeSecurity(ctx, &settings, repo); err != nil {
			return err
		}
		return serveFn(ctx, settings, repo)
	case "export":
		return runExport(ctx, output, settings, repo, *exportPath, *exportPool)
	case "register", "mint":
		return fmt.Errorf("%s moved to external project grok-register (see docs/EXTERNAL_REGISTER.md)", command)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func runBootstrapAdmin(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	repo repository.AdminBootstrapRepository,
) error {
	if output == nil {
		return errors.New("bootstrap-admin output is required")
	}
	password, err := bootstrap.ReadPasswordStdin(input)
	if err != nil {
		return fmt.Errorf("read administrator password: %w", err)
	}
	result, err := bootstrap.NewAdminBootstrapService(repo, time.Now, bcrypt.DefaultCost).Bootstrap(ctx, password)
	if err != nil {
		return fmt.Errorf("bootstrap administrator: %w", err)
	}
	return json.NewEncoder(output).Encode(map[string]any{
		"status":   result.Status,
		"username": result.Admin.Username,
	})
}

func bootstrapServeSecurity(ctx context.Context, settings *config.Config, repo repository.LegacySecurityBootstrapRepository) error {
	if settings == nil {
		return errors.New("security bootstrap settings are required")
	}
	result, err := bootstrap.NewLegacySecurityService(repo, time.Now, bcrypt.DefaultCost).Bootstrap(ctx, bootstrap.LegacySecrets{
		PanelPassword: settings.PanelPassword,
		AppKey:        settings.AppKey,
		APIKey:        settings.APIKey,
	})
	if err != nil {
		return fmt.Errorf("bootstrap legacy security: %w", err)
	}
	settings.PanelPassword = ""
	settings.AppKey = ""
	settings.APIKey = ""
	slog.Info("legacy security bootstrap complete",
		"admin", result.Admin,
		"client_key", result.ClientKey,
		"admin_setup_required", result.AdminSetupRequired,
	)
	return nil
}

func importLegacyWhenEmpty(ctx context.Context, repo *sqlite.SQLite, dataDir string) error {
	count, err := repo.AccountCount(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	legacyPath := filepath.Join(dataDir, "cli_accounts.json")
	if _, err := os.Stat(legacyPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect legacy account file: %w", err)
	}
	imported, err := repo.ImportLegacyJSON(ctx, legacyPath)
	if err != nil {
		return err
	}
	slog.Info("legacy accounts imported", "count", imported)
	return nil
}

func printStatus(ctx context.Context, output io.Writer, repo repository.AccountLister) error {
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		return err
	}
	ready := 0
	unavailable := 0
	reasons := map[string]int{}
	for _, item := range accounts {
		if item.Pool == account.PoolReady {
			ready++
		} else {
			unavailable++
			reasons[string(item.UnavailableReason)]++
		}
	}
	return json.NewEncoder(output).Encode(map[string]any{
		"version":     version,
		"ready":       ready,
		"unavailable": unavailable,
		"reasons":     reasons,
	})
}

func serve(ctx context.Context, settings config.Config, repo runtimeRepository) error {
	frontendFS, err := frontendFileSystem(settings.Frontend.StaticPath)
	if err != nil {
		return err
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		return err
	}
	pool := scheduler.New(accounts)
	if managed, err := repo.GetSettings(ctx); err == nil {
		settings = applyManagedSettings(settings, managed)
		slog.Info("loaded managed settings", "revision", managed.Revision)
	}
	maxConcurrent := settings.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	pool.ApplyMaxActive(maxConcurrent)
	strategy := scheduler.ParseStrategy(settings.Strategy)
	pool.WithStrategy(strategy)
	activeSize := settings.ActiveSize
	if activeSize < 0 {
		activeSize = 0
	}
	pool.ApplyActiveSize(activeSize)
	stickyTTL := time.Duration(settings.StickyTTLMinutes) * time.Minute
	if stickyTTL <= 0 {
		stickyTTL = 30 * time.Minute
	}
	pool.WithSticky(settings.StickyPool, stickyTTL)
	if settings.StickyPool {
		slog.Info("account pool sticky enabled", "ttl", stickyTTL.String())
	}
	maxAttempts := settings.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	slog.Info("account pool",
		"strategy", string(strategy),
		"max_active_per_account", maxConcurrent,
		"max_attempts_per_request", maxAttempts,
		"active_size", activeSize,
	)
	httpClient := &http.Client{Timeout: settings.RequestTimeout()}
	upstreamClient := upstream.NewClientWithOptions(
		settings.ProxyBaseURL,
		settings.ClientVersion,
		httpClient,
		upstream.ClientOptions{
			TokenAuth:        settings.TokenAuth,
			ClientIdentifier: settings.ClientIdentifier,
			UserAgent:        settings.ClientUserAgent,
		},
	)
	gateway := service.NewGateway(
		pool,
		repo,
		upstreamClient,
		service.WithQuotaRetry(time.Duration(settings.QuotaRetryMinutes)*time.Minute),
		service.WithRateRetry(time.Duration(settings.RateRetrySeconds)*time.Second),
		service.WithMaxAttempts(maxAttempts),
		service.WithAcquireTimeout(time.Duration(settings.AcquireTimeoutSec)*time.Second),
		service.WithAuditSink(repo),
	)
	// Optional temporary interceptor: logs client + upstream stages for protocol debugging.
	var apiGateway api.Gateway = gateway
	var tracer *intercept.Tracer
	if settings.DebugTrace {
		traceDir := settings.DebugTraceDir
		if strings.TrimSpace(traceDir) == "" {
			traceDir = filepath.Join(settings.DataDir, "traces")
		}
		tracer = intercept.New(intercept.Options{
			Enabled:    true,
			Dir:        traceDir,
			ErrorsOnly: settings.DebugTraceErrorsOnly,
		})
		apiGateway = &intercept.TraceGateway{Inner: gateway, Tracer: tracer}
		if settings.DebugTraceErrorsOnly {
			slog.Warn("debug_trace enabled (errors only); writing failed-request JSONL", "dir", traceDir)
		} else {
			slog.Warn("debug_trace enabled; writing JSONL traces", "dir", traceDir)
		}
	}
	adminService := admin.NewService(repo, upstreamClient, admin.WithMaintenance(upstreamClient), admin.WithSink(pool))
	// Registration is an external project (grok-register). This service only
	// imports credentials via the admin import API / panel.
	readiness := &api.AtomicReadiness{}
	readiness.Set(false, "starting")
	settingsPtr := &settings
	applier := &runtimeSettingsApplier{pool: pool, gateway: gateway, settings: settingsPtr}
	handler := newAPIHandler(
		*settingsPtr,
		repo,
		apiGateway,
		poolStatusProvider{scheduler: pool},
		adminService,
		frontendFS,
		tracer,
		readiness,
		repo,
		applier,
	)
	// Accept traffic once accounts are loaded into the scheduler. Recovery
	// continues in the background and may still move accounts between pools.
	readiness.Set(true, "serving")
	server := &http.Server{
		Addr:              settings.Address(),
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	recoveryCtx, cancelRecovery := context.WithCancel(ctx)
	defer cancelRecovery()
	recoveryDone := make(chan struct{})
	go func() {
		// Retain request audits for 30 days by default.
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		prune := func() {
			days := 30
			if managed, err := repo.GetSettings(context.Background()); err == nil && managed.Audit.RetentionDays > 0 {
				days = managed.Audit.RetentionDays
			}
			cut := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
			n, err := repo.PruneRequestAudits(context.Background(), cut)
			if err != nil {
				slog.Warn("prune request audits failed", "error", err)
				return
			}
			if n > 0 {
				slog.Info("pruned request audits", "deleted", n, "older_than", cut.Format(time.RFC3339))
			}
		}
		prune()
		for {
			select {
			case <-recoveryCtx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
	go func() {
		defer close(recoveryDone)
		// RunRecovery only exits on context cancel; per-account errors are logged.
		// 10s ticks + parallel quota/validating probes clear multi-thousand
		// due backlogs much faster than the old sequential 20s/32 rate.
		if err := runtimeworker.RunRecovery(
			recoveryCtx,
			pool,
			repo,
			10*time.Second,
			runtimeworker.WithCredentialRecovery(repo, upstreamClient, upstreamClient),
			runtimeworker.WithQuotaProber(upstreamClient),
			runtimeworker.WithQuotaRetry(time.Duration(settings.QuotaRetryMinutes)*time.Minute),
		); err != nil {
			slog.Error("recovery worker stopped", "error", err)
		}
	}()
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("grok2api Go server starting", "address", settings.Address(), "version", version)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		cancelRecovery()
		<-recoveryDone
		return err
	case err := <-serverErrors:
		cancelRecovery()
		<-recoveryDone
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func newAPIHandler(
	settings config.Config,
	repo runtimeRepository,
	gateway api.Gateway,
	status api.StatusProvider,
	adminService api.AdminService,
	frontendFS fs.FS,
	tracer *intercept.Tracer,
	readiness api.Readiness,
	audits api.AuditReader,
	settingsApplier api.SettingsApplier,
) http.Handler {
	adminAuthService := adminauth.NewService(repo)
	clientAccess := service.NewClientAccess(repo)
	clientKeyService := clientkeys.NewService(repo)
	serverOptions := []api.Option{
		api.WithDefaultModel(settings.DefaultModel),
		api.WithAdmin(adminService, ""),
		api.WithAdminAuth(adminAuthService, api.AdminAuthHandlerOptions{SecureCookies: settings.AdminSecureCookies}),
		api.WithClientAccess(clientAccess),
		api.WithClientKeys(clientKeyService),
		api.WithReadiness(readiness),
		api.WithAuditReader(audits),
		api.WithModelAdmin(repo),
		api.WithSettingsAdmin(repo),
		api.WithSettingsApplier(settingsApplier),
	}
	if frontendFS != nil {
		serverOptions = append(serverOptions, api.WithFrontend(frontendFS))
	}
	if tracer != nil {
		serverOptions = append(serverOptions, api.WithDebugTrace(tracer))
	}
	if models, err := repo.ListModels(context.Background(), false); err == nil {
		serverOptions = append(serverOptions, api.WithModelCatalog(sqlite.CatalogFromRegistry(models)))
	}
	return api.NewServer(gateway, status, "", serverOptions...).Handler()
}

func frontendFileSystem(staticPath string) (fs.FS, error) {
	staticPath = strings.TrimSpace(staticPath)
	if staticPath == "" {
		return nil, nil
	}
	frontendFS := os.DirFS(filepath.Clean(staticPath))
	info, err := fs.Stat(frontendFS, "index.html")
	if err != nil {
		return nil, fmt.Errorf("validate frontend static path %q: index.html: %w", staticPath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("validate frontend static path %q: index.html is a directory", staticPath)
	}
	return frontendFS, nil
}

type poolStatusProvider struct {
	scheduler *scheduler.Scheduler
}

func (p poolStatusProvider) PoolStatus() api.PoolStatus {
	ready, unavailable, reasons := p.scheduler.Status()
	converted := make(map[string]int, len(reasons))
	for reason, count := range reasons {
		converted[string(reason)] = count
	}
	return api.PoolStatus{
		Ready:       ready,
		Unavailable: unavailable,
		Reasons:     converted,
	}
}

func (p poolStatusProvider) ActiveByID() map[string]int {
	return p.scheduler.ActiveByID()
}

// exportAccount mirrors admin.ImportAccount so the emitted file can be pasted
// back into /panel import (or another grok2api instance) without field drift.
type exportAccount struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	Email        string `json:"email,omitempty"`
	OIDCIssuer   string `json:"oidc_issuer,omitempty"`
	OIDCClientID string `json:"oidc_client_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	Pool         string `json:"pool"`
	CreatedAt    string `json:"created_at,omitempty"`
}

func runExport(
	ctx context.Context,
	output io.Writer,
	settings config.Config,
	repo repository.AccountLister,
	outPath string,
	poolFilter string,
) error {
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	wantPool := account.Pool(strings.TrimSpace(poolFilter))
	filtered := make([]account.Account, 0, len(accounts))
	for _, a := range accounts {
		if wantPool != "" && a.Pool != wantPool {
			continue
		}
		filtered = append(filtered, a)
	}

	exported := make([]exportAccount, 0, len(filtered))
	var withRefresh int
	for _, a := range filtered {
		item := exportAccount{
			ID:           a.ID,
			Key:          a.AccessToken,
			RefreshToken: a.RefreshToken,
			Email:        a.Email,
			OIDCIssuer:   a.OIDCIssuer,
			OIDCClientID: a.OIDCClientID,
			UserID:       a.UserID,
			TeamID:       a.TeamID,
			Pool:         string(a.Pool),
			CreatedAt:    a.CreatedAt.Format(time.RFC3339),
		}
		if a.ExpiresAt.IsZero() {
			if a.AccessToken != "" {
				item.ExpiresIn = 3600
			}
		} else {
			remaining := max(0, int(time.Until(a.ExpiresAt).Round(time.Second)/time.Second))
			item.ExpiresIn = remaining
			item.ExpiresAt = a.ExpiresAt.Format(time.RFC3339)
		}
		if a.RefreshToken != "" {
			withRefresh++
		}
		exported = append(exported, item)
	}

	if strings.TrimSpace(outPath) == "" {
		outPath = filepath.Join(settings.DataDir, "export_accounts.json")
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return fmt.Errorf("create export dir: %w", err)
	}
	data, err := json.MarshalIndent(exported, "", "  ")
	if err != nil {
		return fmt.Errorf("encode export: %w", err)
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return fmt.Errorf("write export: %w", err)
	}

	summary := map[string]any{
		"total":        len(filtered),
		"with_refresh": withRefresh,
		"pool_filter":  string(wantPool),
		"output":       outPath,
	}
	_ = json.NewEncoder(output).Encode(summary)
	return nil
}

func applyManagedSettings(base config.Config, managed settings.Document) config.Config {
	base.MaxConcurrent = managed.Pool.MaxConcurrent
	base.MaxAttempts = managed.Pool.MaxAttempts
	base.Strategy = managed.Pool.Strategy
	base.ActiveSize = managed.Pool.ActiveSize
	base.StickyPool = managed.Pool.Sticky
	base.StickyTTLMinutes = managed.Pool.StickyTTLMinutes
	base.QuotaRetryMinutes = managed.Pool.QuotaRetryMinutes
	base.RateRetrySeconds = managed.Pool.RateRetrySeconds
	base.RequestTimeoutSec = managed.Timeouts.RequestTimeoutSec
	base.AcquireTimeoutSec = managed.Timeouts.AcquireTimeoutSec
	return base
}

type runtimeSettingsApplier struct {
	pool     *scheduler.Scheduler
	gateway  *service.Gateway
	settings *config.Config
}

func (a *runtimeSettingsApplier) ApplySettings(doc settings.Document) error {
	if a == nil || a.pool == nil || a.settings == nil {
		return nil
	}
	*a.settings = applyManagedSettings(*a.settings, doc)
	a.pool.ApplyMaxActive(a.settings.MaxConcurrent)
	a.pool.WithStrategy(scheduler.ParseStrategy(a.settings.Strategy))
	a.pool.ApplyActiveSize(a.settings.ActiveSize)
	stickyTTL := time.Duration(a.settings.StickyTTLMinutes) * time.Minute
	a.pool.WithSticky(a.settings.StickyPool, stickyTTL)
	if a.gateway != nil {
		a.gateway.ConfigureRuntime(
			time.Duration(a.settings.QuotaRetryMinutes)*time.Minute,
			time.Duration(a.settings.RateRetrySeconds)*time.Second,
			time.Duration(a.settings.AcquireTimeoutSec)*time.Second,
			a.settings.MaxAttempts,
		)
	}
	slog.Info("applied managed settings", "revision", doc.Revision)
	return nil
}
