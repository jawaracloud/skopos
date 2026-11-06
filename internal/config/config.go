// Package config loads gateway configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jawaracloud/skopos/internal/plan"
)

// Mode selects which part of the query path this process serves.
type Mode string

const (
	// ModeAll serves the query heads and the public API in one process.
	ModeAll Mode = "all"
	// ModeQuery serves only the query heads. It is the query worker role.
	ModeQuery Mode = "query"
	// ModeWorker is an explicit alias for ModeQuery, documented as the query
	// worker mode.
	ModeWorker Mode = "worker"
	// ModeFrontend serves health/version locally and proxies query traffic to
	// one or more query workers.
	ModeFrontend Mode = "frontend"
)

// ServeQueries reports whether the mode registers the Prometheus, Tempo, and
// Loki query heads.
func (m Mode) ServeQueries() bool {
	return m == ModeAll || m == ModeQuery || m == ModeWorker
}

// Config is the gateway runtime configuration.
type Config struct {
	Mode               Mode
	ListenAddr         string
	ClickHouseURL      string
	ClickHouseDatabase string
	ClickHouseUser     string
	ClickHousePassword string
	Tables             plan.TableNames
	MaxExecutionTime   time.Duration
	MaxMemoryUsage     uint64
	MaxRowsToRead      uint64
	MaxResultRows      uint64
	MaxScanRows        int
	MaxQueryRange      time.Duration
	MaxResponseBytes   int64
	ReadinessTimeout   time.Duration
	MaxRequestBytes    int64
	QueryURLs          []string
	QueryTimeout       time.Duration
	AuthEnabled        bool
	AuthUsername       string
	AuthPassword       string
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
		Mode:               ModeAll,
		ListenAddr:         ":8080",
		ClickHouseURL:      "http://127.0.0.1:8123",
		ClickHouseDatabase: "otel",
		ClickHouseUser:     "default",
		Tables:             plan.DefaultTableNames(),
		MaxExecutionTime:   30 * time.Second,
		MaxMemoryUsage:     1 << 30,
		MaxRowsToRead:      10_000_000,
		MaxResultRows:      100_000,
		MaxScanRows:        200_000,
		MaxQueryRange:      7 * 24 * time.Hour,
		MaxResponseBytes:   64 << 20,
		ReadinessTimeout:   2 * time.Second,
		MaxRequestBytes:    1 << 20,
		QueryTimeout:       30 * time.Second,
		LogLevel:           slog.LevelInfo,
	}

	mode, err := stringEnv(lookup, "SKOPOS_MODE", string(ModeAll))
	if err != nil {
		return Config{}, err
	}
	if cfg.Mode, err = ParseMode(mode); err != nil {
		return Config{}, err
	}

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
	if cfg.Tables.Traces, err = tableEnv(lookup, "SKOPOS_TABLE_TRACES", cfg.Tables.Traces); err != nil {
		return Config{}, err
	}
	if cfg.Tables.Logs, err = tableEnv(lookup, "SKOPOS_TABLE_LOGS", cfg.Tables.Logs); err != nil {
		return Config{}, err
	}
	if cfg.Tables.MetricsGauge, err = tableEnv(lookup, "SKOPOS_TABLE_METRICS_GAUGE", cfg.Tables.MetricsGauge); err != nil {
		return Config{}, err
	}
	if cfg.Tables.MetricsSum, err = tableEnv(lookup, "SKOPOS_TABLE_METRICS_SUM", cfg.Tables.MetricsSum); err != nil {
		return Config{}, err
	}
	if cfg.Tables.MetricsHistogram, err = tableEnv(lookup, "SKOPOS_TABLE_METRICS_HISTOGRAM", cfg.Tables.MetricsHistogram); err != nil {
		return Config{}, err
	}
	if cfg.Tables.MetricsExponentialHistogram, err = tableEnv(lookup, "SKOPOS_TABLE_METRICS_EXPONENTIAL_HISTOGRAM", cfg.Tables.MetricsExponentialHistogram); err != nil {
		return Config{}, err
	}
	if cfg.Tables.MetricsSummary, err = tableEnv(lookup, "SKOPOS_TABLE_METRICS_SUMMARY", cfg.Tables.MetricsSummary); err != nil {
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
	if cfg.MaxScanRows, err = intValueEnv(lookup, "SKOPOS_MAX_SCAN_ROWS", cfg.MaxScanRows); err != nil {
		return Config{}, err
	}
	if cfg.MaxQueryRange, err = durationEnv(lookup, "SKOPOS_MAX_QUERY_RANGE", cfg.MaxQueryRange); err != nil {
		return Config{}, err
	}
	if cfg.MaxResponseBytes, err = intEnv(lookup, "SKOPOS_CLICKHOUSE_MAX_RESPONSE_BYTES", cfg.MaxResponseBytes); err != nil {
		return Config{}, err
	}
	if cfg.ReadinessTimeout, err = durationEnv(lookup, "SKOPOS_READINESS_TIMEOUT", cfg.ReadinessTimeout); err != nil {
		return Config{}, err
	}
	if cfg.QueryTimeout, err = durationEnv(lookup, "SKOPOS_QUERY_TIMEOUT", cfg.QueryTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBytes, err = intEnv(lookup, "SKOPOS_MAX_REQUEST_BYTES", cfg.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	if cfg.AuthEnabled, err = boolEnv(lookup, "SKOPOS_AUTH_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.AuthUsername, err = stringEnv(lookup, "SKOPOS_AUTH_USERNAME", ""); err != nil {
		return Config{}, err
	}
	if cfg.AuthPassword, err = stringEnv(lookup, "SKOPOS_AUTH_PASSWORD", ""); err != nil {
		return Config{}, err
	}
	if cfg.Mode == ModeFrontend {
		raw, err := stringEnv(lookup, "SKOPOS_QUERY_URLS", "")
		if err != nil {
			return Config{}, err
		}
		cfg.QueryURLs, err = ParseQueryURLs(raw)
		if err != nil {
			return Config{}, err
		}
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
	if cfg.QueryTimeout <= 0 {
		return Config{}, fmt.Errorf("SKOPOS_QUERY_TIMEOUT: must be positive")
	}
	if cfg.Mode == ModeFrontend && len(cfg.QueryURLs) == 0 {
		return Config{}, fmt.Errorf("SKOPOS_QUERY_URLS: at least one URL is required in frontend mode")
	}
	if cfg.AuthEnabled && (cfg.AuthUsername == "" || cfg.AuthPassword == "") {
		return Config{}, fmt.Errorf("SKOPOS_AUTH_ENABLED requires SKOPOS_AUTH_USERNAME and SKOPOS_AUTH_PASSWORD")
	}
	if cfg.MaxMemoryUsage == 0 || cfg.MaxRowsToRead == 0 || cfg.MaxResultRows == 0 ||
		cfg.MaxScanRows <= 0 || cfg.MaxResponseBytes <= 0 || cfg.MaxRequestBytes <= 0 {
		return Config{}, fmt.Errorf("SKOPOS limits must be positive")
	}
	if cfg.MaxQueryRange <= 0 {
		return Config{}, fmt.Errorf("SKOPOS_MAX_QUERY_RANGE: must be positive")
	}
	return cfg, nil
}

// ParseMode parses SKOPOS_MODE. An empty value means ModeAll.
func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(ModeAll):
		return ModeAll, nil
	case string(ModeQuery):
		return ModeQuery, nil
	case string(ModeWorker):
		return ModeWorker, nil
	case string(ModeFrontend):
		return ModeFrontend, nil
	default:
		return "", fmt.Errorf("SKOPOS_MODE: unsupported mode %q (want all, query, worker, or frontend)", raw)
	}
}

// ParseQueryURLs parses and validates the comma-separated SKOPOS_QUERY_URLS
// value. Each entry must be an absolute http(s) base URL. Empty entries are
// ignored; trailing slashes are normalized away.
func ParseQueryURLs(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil {
			return nil, fmt.Errorf("SKOPOS_QUERY_URLS: invalid URL %q: %w", part, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("SKOPOS_QUERY_URLS: %q must use http or https", part)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("SKOPOS_QUERY_URLS: %q has no host", part)
		}
		if u.Fragment != "" {
			return nil, fmt.Errorf("SKOPOS_QUERY_URLS: %q must not contain a fragment", part)
		}
		u.Path = strings.TrimRight(u.Path, "/")
		out = append(out, u.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SKOPOS_QUERY_URLS: at least one URL is required in frontend mode")
	}
	return out, nil
}

func stringEnv(lookup func(string) (string, bool), key, fallback string) (string, error) {
	if v, ok := lookup(key); ok {
		return v, nil
	}
	return fallback, nil
}

func tableEnv(lookup func(string) (string, bool), key, fallback string) (string, error) {
	v, err := stringEnv(lookup, key, fallback)
	if err != nil {
		return "", err
	}
	if !plan.ValidIdent(v) {
		return "", fmt.Errorf("%s: invalid table identifier %q", key, v)
	}
	return v, nil
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

// intValueEnv parses an environment value that must fit in int. It avoids an
// unchecked uint64-to-int conversion at the call sites.
func intValueEnv(lookup func(string) (string, bool), key string, fallback int) (int, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func boolEnv(lookup func(string) (string, bool), key string, fallback bool) (bool, error) {
	v, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
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
