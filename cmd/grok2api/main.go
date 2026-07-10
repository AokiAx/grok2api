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
	"syscall"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/api"
	"github.com/AokiAx/grok2api/internal/config"
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
	handler := api.NewServer(
		gateway,
		poolStatusProvider{scheduler: pool},
		settings.APIKey,
		api.WithDefaultModel(settings.DefaultModel),
	).Handler()
	server := &http.Server{
		Addr:              settings.Address(),
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	recoveryCtx, cancelRecovery := context.WithCancel(ctx)
	defer cancelRecovery()
	recoveryErrors := make(chan error, 1)
	go func() {
		recoveryErrors <- runtimeworker.RunRecovery(
			recoveryCtx,
			pool,
			repo,
			time.Minute,
		)
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
		return server.Shutdown(shutdownCtx)
	case err := <-recoveryErrors:
		if err != nil {
			return fmt.Errorf("recovery worker: %w", err)
		}
		return nil
	case err := <-serverErrors:
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
