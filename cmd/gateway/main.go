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

	"github.com/obi-wrapper/skopos/internal/chclient"
	"github.com/obi-wrapper/skopos/internal/config"
	"github.com/obi-wrapper/skopos/internal/httpapi"
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

	api, err := httpapi.New(httpapi.Config{
		Logger:           logger,
		Pinger:           client,
		Version:          version,
		ReadinessTimeout: cfg.ReadinessTimeout,
		MaxRequestBytes:  cfg.MaxRequestBytes,
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
