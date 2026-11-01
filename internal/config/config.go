// Package config loads gateway configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/obi-wrapper/skopos/internal/plan"
)

// Config is the gateway runtime configuration.
type Config struct {
	ListenAddr         string
	ClickHouseURL      string
	ClickHouseDatabase string
	ClickHouseUser     string
	ClickHousePassword string
	MaxExecutionTime   time.Duration
	MaxMemoryUsage     uint64
	MaxRowsToRead      uint64
	MaxResultRows      uint64
	MaxResponseBytes   int64
	ReadinessTimeout   time.Duration
	MaxRequestBytes    int64
	LogLevel           slog.Level
}

// FromEnv loads configuration from the process environment.
func FromEnv() (Config, error) {
	return FromLookup(os.LookupEnv)
}

// FromLookup loads configuration using lookup. It exists so tests do not need
// to mutate the process environment.
func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		ListenAddr:         ":8080",
		ClickHouseURL:      "http://127.0.0.1:8123",
		ClickHouseDatabase: "otel",
		ClickHouseUser:     "default",
		MaxExecutionTime:   30 * time.Second,
		MaxMemoryUsage:     1 << 30,
		MaxRowsToRead:      10_000_000,
		MaxResultRows:      100_000,
		MaxResponseBytes:   64 << 20,
		ReadinessTimeout:   2 * time.Second,
		MaxRequestBytes:    1 << 20,
		LogLevel:           slog.LevelInfo,
	}

	var err error
	if cfg.ListenAddr, err = stringEnv(lookup, "SKOPOS_LISTEN_ADDR", cfg.ListenAddr); err != nil {
		return Config{}, err
	}
	if cfg.ClickHouseURL, err = stringEnv(lookup, "SKOPOS_CLICKHOUSE_URL", cfg.ClickHouseURL); err != nil {
		return Config{}, err
	}
	if cfg.ClickHouseDatabase, err = stringEnv(lookup, "SKOPOS_CLICKHOUSE_DATABASE", cfg.ClickHouseDatabase); err != nil {
		return Config{}, err
	}
	if cfg.ClickHouseUser, err = stringEnv(lookup, "SKOPOS_CLICKHOUSE_USER", cfg.ClickHouseUser); err != nil {
		return Config{}, err
	}
	if cfg.ClickHousePassword, err = stringEnv(lookup, "SKOPOS_CLICKHOUSE_PASSWORD", cfg.ClickHousePassword); err != nil {
		return Config{}, err
	}
	if cfg.MaxExecutionTime, err = durationEnv(lookup, "SKOPOS_CLICKHOUSE_MAX_EXECUTION_TIME", cfg.MaxExecutionTime); err != nil {
		return Config{}, err
	}
	if cfg.MaxMemoryUsage, err = uintEnv(lookup, "SKOPOS_CLICKHOUSE_MAX_MEMORY_USAGE", cfg.MaxMemoryUsage); err != nil {
		return Config{}, err
	}
	if cfg.MaxRowsToRead, err = uintEnv(lookup, "SKOPOS_CLICKHOUSE_MAX_ROWS_TO_READ", cfg.MaxRowsToRead); err != nil {
		return Config{}, err
	}
	if cfg.MaxResultRows, err = uintEnv(lookup, "SKOPOS_CLICKHOUSE_MAX_RESULT_ROWS", cfg.MaxResultRows); err != nil {
		return Config{}, err
	}
	if cfg.MaxResponseBytes, err = intEnv(lookup, "SKOPOS_CLICKHOUSE_MAX_RESPONSE_BYTES", cfg.MaxResponseBytes); err != nil {
		return Config{}, err
	}
	if cfg.ReadinessTimeout, err = durationEnv(lookup, "SKOPOS_READINESS_TIMEOUT", cfg.ReadinessTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBytes, err = intEnv(lookup, "SKOPOS_MAX_REQUEST_BYTES", cfg.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	level, err := stringEnv(lookup, "SKOPOS_LOG_LEVEL", "info")
	if err != nil {
		return Config{}, err
	}
	switch strings.ToLower(level) {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "info":
		cfg.LogLevel = slog.LevelInfo
	case "warn", "warning":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, fmt.Errorf("SKOPOS_LOG_LEVEL: unsupported level %q", level)
	}

	if cfg.ListenAddr == "" {
		return Config{}, fmt.Errorf("SKOPOS_LISTEN_ADDR: must not be empty")
	}
	if !plan.ValidIdent(cfg.ClickHouseDatabase) {
		return Config{}, fmt.Errorf("SKOPOS_CLICKHOUSE_DATABASE: invalid identifier %q", cfg.ClickHouseDatabase)
	}
	if cfg.MaxExecutionTime <= 0 {
		return Config{}, fmt.Errorf("SKOPOS_CLICKHOUSE_MAX_EXECUTION_TIME: must be positive")
	}
	if cfg.ReadinessTimeout <= 0 {
		return Config{}, fmt.Errorf("SKOPOS_READINESS_TIMEOUT: must be positive")
	}
	if cfg.MaxMemoryUsage == 0 || cfg.MaxRowsToRead == 0 || cfg.MaxResultRows == 0 ||
		cfg.MaxResponseBytes <= 0 || cfg.MaxRequestBytes <= 0 {
		return Config{}, fmt.Errorf("SKOPOS limits must be positive")
	}
	return cfg, nil
}

func stringEnv(lookup func(string) (string, bool), key, fallback string) (string, error) {
	if v, ok := lookup(key); ok {
		return v, nil
	}
	return fallback, nil
}

func durationEnv(lookup func(string) (string, bool), key string, fallback time.Duration) (time.Duration, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func uintEnv(lookup func(string) (string, bool), key string, fallback uint64) (uint64, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func intEnv(lookup func(string) (string, bool), key string, fallback int64) (int64, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
