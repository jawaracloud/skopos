package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrofisr/skopos/internal/chclient"
	"github.com/mrofisr/skopos/internal/config"
	"github.com/mrofisr/skopos/internal/httpapi"
	"github.com/mrofisr/skopos/internal/loki"
	"github.com/mrofisr/skopos/internal/plan"
	"github.com/mrofisr/skopos/internal/prom"
	"github.com/mrofisr/skopos/internal/tempo"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	client, err := chclient.New(chclient.Config{
		BaseURL:          cfg.ClickHouseURL,
		Database:         cfg.ClickHouseDatabase,
		User:             cfg.ClickHouseUser,
		Password:         cfg.ClickHousePassword,
		Timeout:          cfg.MaxExecutionTime,
		MaxExecutionTime: cfg.MaxExecutionTime,
		MaxMemoryUsage:   cfg.MaxMemoryUsage,
		MaxRowsToRead:    cfg.MaxRowsToRead,
		MaxResultRows:    cfg.MaxResultRows,
		MaxResponseBytes: cfg.MaxResponseBytes,
	})
	if err != nil {
		return fmt.Errorf("clickhouse client: %w", err)
	}
	defer client.Close()

	schema := plan.DefaultSchema(cfg.ClickHouseDatabase, cfg.Tables)
	fetcher, err := prom.NewClickHouseFetcher(client, schema, cfg.Tables, int(cfg.MaxScanRows))
	if err != nil {
		return fmt.Errorf("prometheus storage: %w", err)
	}
	promEngine := prom.NewEngine(fetcher)
	promEngine.MaxSamples = int(cfg.MaxScanRows)
	promAPI := prom.NewAPI(promEngine, fetcher, version)
	promAPI.MaxRange = cfg.MaxQueryRange

	tempoFetcher, err := tempo.NewClickHouseFetcher(client, schema, cfg.Tables, int(cfg.MaxScanRows))
	if err != nil {
		return fmt.Errorf("tempo storage: %w", err)
	}
	tempoAPI := tempo.NewAPI(tempoFetcher, version)
	tempoAPI.MaxRange = cfg.MaxQueryRange

	lokiFetcher, err := loki.NewClickHouseFetcher(client, schema, cfg.Tables, int(cfg.MaxScanRows))
	if err != nil {
		return fmt.Errorf("loki storage: %w", err)
	}
	lokiAPI := loki.NewAPI(lokiFetcher, version)
	lokiAPI.MaxRange = cfg.MaxQueryRange

	register := func(mux *http.ServeMux) {
		promAPI.Register(mux)
		tempoAPI.Register(mux)
		lokiAPI.Register(mux)
	}

	api, err := httpapi.New(httpapi.Config{
		Logger:           logger,
		Pinger:           client,
		Version:          version,
		ReadinessTimeout: cfg.ReadinessTimeout,
		MaxRequestBytes:  cfg.MaxRequestBytes,
		Register:         register,
	})
	if err != nil {
		return fmt.Errorf("http api: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting gateway",
		"version", version,
		"addr", cfg.ListenAddr,
		"clickhouse_url", redactURL(cfg.ClickHouseURL),
		"database", cfg.ClickHouseDatabase,
		"read_only_user", cfg.ClickHouseUser,
		"tables", cfg.Tables,
		"promql", true,
		"traceql", true,
		"logql", true,
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}
