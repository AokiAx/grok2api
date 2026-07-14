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

	"github.com/AokiAx/grok2api/backend/internal/account"
	"github.com/AokiAx/grok2api/backend/internal/admin"
	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/config"
	"github.com/AokiAx/grok2api/backend/internal/intercept"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	runtimeworker "github.com/AokiAx/grok2api/backend/internal/runtime"
	"github.com/AokiAx/grok2api/backend/internal/scheduler"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"github.com/AokiAx/grok2api/backend/internal/service"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

const version = "1.0.0-go"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		slog.Error("grok2api stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
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
	if err := flags.Parse(arguments); err != nil {
		return err
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
	repo, err := repository.OpenSQLiteWithCipher(ctx, filepath.Join(settings.DataDir, "grok2api.db"), credCipher)
	if err != nil {
		return err
	}
	defer repo.Close()
	if err := importLegacyWhenEmpty(ctx, repo, settings.DataDir); err != nil {
		return err
	}

	switch command {
	case "migrate", "status":
		return printStatus(ctx, output, repo)
	case "serve":
		return serve(ctx, settings, repo)
	case "export":
		return runExport(ctx, output, settings, repo, *exportPath, *exportPool)
	case "register", "mint":
		return fmt.Errorf("%s moved to external project grok-register (see docs/EXTERNAL_REGISTER.md)", command)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func importLegacyWhenEmpty(ctx context.Context, repo *repository.SQLite, dataDir string) error {
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

func printStatus(ctx context.Context, output io.Writer, repo *repository.SQLite) error {
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

func serve(ctx context.Context, settings config.Config, repo *repository.SQLite) error {
	frontendFS, err := frontendFileSystem(settings.Frontend.StaticPath)
	if err != nil {
		return err
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		return err
	}
	pool := scheduler.New(accounts)
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
	adminService := admin.NewService(repo, upstreamClient, admin.WithSink(pool))
	// Registration is an external project (grok-register). This service only
	// imports credentials via the admin import API / panel.
	serverOptions := []api.Option{
		api.WithDefaultModel(settings.DefaultModel),
		api.WithAdmin(adminService, settings.AdminKey()),
	}
	if frontendFS != nil {
		serverOptions = append(serverOptions, api.WithFrontend(frontendFS))
	}
	if tracer != nil {
		serverOptions = append(serverOptions, api.WithDebugTrace(tracer))
	}
	handler := api.NewServer(
		apiGateway,
		poolStatusProvider{scheduler: pool},
		settings.APIKey,
		serverOptions...,
	).Handler()
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
	repo *repository.SQLite,
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
