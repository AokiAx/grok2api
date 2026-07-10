package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
	"github.com/AokiAx/grok2api/internal/api"
	"github.com/AokiAx/grok2api/internal/config"
	"github.com/AokiAx/grok2api/internal/register"
	regsettings "github.com/AokiAx/grok2api/internal/register/settings"
	"github.com/AokiAx/grok2api/internal/repository"
	runtimeworker "github.com/AokiAx/grok2api/internal/runtime"
	"github.com/AokiAx/grok2api/internal/scheduler"
	"github.com/AokiAx/grok2api/internal/service"
	"github.com/AokiAx/grok2api/internal/upstream"
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
	count := flags.Int("count", 0, "register account count")
	workers := flags.Int("workers", 0, "register worker concurrency")
	dryRun := flags.Bool("dry-run", false, "register without persisting accounts")
	proxyURL := flags.String("proxy", "", "override proxy URL for register/mint")
	ssoCookie := flags.String("sso-cookie", "", "SSO cookie for mint command")
	email := flags.String("email", "", "email metadata for mint command")
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
	repo, err := repository.OpenSQLite(ctx, filepath.Join(settings.DataDir, "grok2api.db"))
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
	case "register":
		return runRegister(ctx, output, settings, repo, *count, *workers, *dryRun, *proxyURL)
	case "mint":
		return runMint(ctx, output, settings, repo, *ssoCookie, *email, *dryRun)
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
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		return err
	}
	pool := scheduler.New(accounts)
	httpClient := &http.Client{Timeout: settings.RequestTimeout()}
	upstreamClient := upstream.NewClient(
		settings.ProxyBaseURL,
		settings.ClientVersion,
		httpClient,
	)
	gateway := service.NewGateway(
		pool,
		repo,
		upstreamClient,
		service.WithQuotaRetry(time.Duration(settings.QuotaRetryMinutes)*time.Minute),
		service.WithRateRetry(time.Duration(settings.RateRetrySeconds)*time.Second),
	)
	adminService := admin.NewService(repo, upstreamClient, admin.WithSink(pool))
	registerStore, err := regsettings.NewStore(settings.DataDir, settings)
	if err != nil {
		return err
	}
	registerPipeline := register.NewPipelineFromSource(registerStore, adminService)
	registerJobs := register.NewJobManagerFromSource(registerStore, registerPipeline)
	handler := api.NewServer(
		gateway,
		poolStatusProvider{scheduler: pool},
		settings.APIKey,
		api.WithDefaultModel(settings.DefaultModel),
		api.WithAdmin(adminService, settings.AdminKey()),
		api.WithRegisterJobs(registerJobs),
		api.WithRegisterSettings(registerStore),
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
		if err := runtimeworker.RunRecovery(
			recoveryCtx,
			pool,
			repo,
			time.Minute,
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

func runRegister(
	ctx context.Context,
	output io.Writer,
	settings config.Config,
	repo *repository.SQLite,
	count, workers int,
	dryRun bool,
	proxyURL string,
) error {
	httpClient := &http.Client{Timeout: settings.RequestTimeout()}
	upstreamClient := upstream.NewClient(settings.ProxyBaseURL, settings.ClientVersion, httpClient)
	adminService := admin.NewService(repo, upstreamClient)
	registerStore, err := regsettings.NewStore(settings.DataDir, settings)
	if err != nil {
		return err
	}
	pipeline := register.NewPipelineFromSource(registerStore, adminService)
	summary, err := pipeline.Run(ctx, register.RunConfig{
		Count:    count,
		Workers:  workers,
		DryRun:   dryRun,
		ProxyURL: proxyURL,
	}, func(message string) {
		slog.Info("register", "event", message)
	})
	if encodeErr := json.NewEncoder(output).Encode(summary); encodeErr != nil {
		return encodeErr
	}
	if err != nil {
		return err
	}
	if summary.Failed > 0 {
		return fmt.Errorf("register finished with %d failures", summary.Failed)
	}
	return nil
}

func runMint(
	ctx context.Context,
	output io.Writer,
	settings config.Config,
	repo *repository.SQLite,
	ssoCookie, email string,
	dryRun bool,
) error {
	if strings.TrimSpace(ssoCookie) == "" {
		return fmt.Errorf("mint requires --sso-cookie")
	}
	httpClient := &http.Client{Timeout: settings.RequestTimeout()}
	upstreamClient := upstream.NewClient(settings.ProxyBaseURL, settings.ClientVersion, httpClient)
	adminService := admin.NewService(repo, upstreamClient)
	registerStore, err := regsettings.NewStore(settings.DataDir, settings)
	if err != nil {
		return err
	}
	pipeline := register.NewPipelineFromSource(registerStore, adminService)
	outcome, err := pipeline.MintSSO(ctx, ssoCookie, email, dryRun)
	if encodeErr := json.NewEncoder(output).Encode(outcome); encodeErr != nil {
		return encodeErr
	}
	return err
}
