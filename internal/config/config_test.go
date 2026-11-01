package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := FromLookup(lookupFrom(nil))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.ClickHouseURL != "http://127.0.0.1:8123" {
		t.Errorf("ClickHouseURL = %q", cfg.ClickHouseURL)
	}
	if cfg.ClickHouseDatabase != "otel" {
		t.Errorf("ClickHouseDatabase = %q", cfg.ClickHouseDatabase)
	}
	if cfg.MaxExecutionTime != 30*time.Second {
		t.Errorf("MaxExecutionTime = %s", cfg.MaxExecutionTime)
	}
	if cfg.MaxMemoryUsage != 1<<30 {
		t.Errorf("MaxMemoryUsage = %d", cfg.MaxMemoryUsage)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := FromLookup(lookupFrom(map[string]string{
		"SKOPOS_LISTEN_ADDR":                   "127.0.0.1:9090",
		"SKOPOS_CLICKHOUSE_URL":                "https://clickhouse.example:8443",
		"SKOPOS_CLICKHOUSE_DATABASE":           "observability",
		"SKOPOS_CLICKHOUSE_USER":               "gateway",
		"SKOPOS_CLICKHOUSE_PASSWORD":           "secret",
		"SKOPOS_CLICKHOUSE_MAX_EXECUTION_TIME": "45s",
		"SKOPOS_CLICKHOUSE_MAX_MEMORY_USAGE":   "2048",
		"SKOPOS_CLICKHOUSE_MAX_ROWS_TO_READ":   "3000",
		"SKOPOS_CLICKHOUSE_MAX_RESULT_ROWS":    "4000",
		"SKOPOS_CLICKHOUSE_MAX_RESPONSE_BYTES": "5000",
		"SKOPOS_READINESS_TIMEOUT":             "3s",
		"SKOPOS_MAX_REQUEST_BYTES":             "6000",
		"SKOPOS_LOG_LEVEL":                     "debug",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" || cfg.ClickHouseDatabase != "observability" ||
		cfg.ClickHouseUser != "gateway" || cfg.ClickHousePassword != "secret" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.MaxExecutionTime != 45*time.Second || cfg.MaxMemoryUsage != 2048 ||
		cfg.MaxRowsToRead != 3000 || cfg.MaxResultRows != 4000 || cfg.MaxResponseBytes != 5000 {
		t.Fatalf("unexpected limits: %#v", cfg)
	}
	if cfg.ReadinessTimeout != 3*time.Second || cfg.MaxRequestBytes != 6000 {
		t.Fatalf("unexpected server limits: %#v", cfg)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"duration", "SKOPOS_CLICKHOUSE_MAX_EXECUTION_TIME", "soon", "SKOPOS_CLICKHOUSE_MAX_EXECUTION_TIME"},
		{"memory", "SKOPOS_CLICKHOUSE_MAX_MEMORY_USAGE", "-1", "SKOPOS_CLICKHOUSE_MAX_MEMORY_USAGE"},
		{"rows", "SKOPOS_CLICKHOUSE_MAX_ROWS_TO_READ", "many", "SKOPOS_CLICKHOUSE_MAX_ROWS_TO_READ"},
		{"log level", "SKOPOS_LOG_LEVEL", "verbose", "SKOPOS_LOG_LEVEL"},
		{"database", "SKOPOS_CLICKHOUSE_DATABASE", "bad-name", "SKOPOS_CLICKHOUSE_DATABASE"},
		{"empty listen", "SKOPOS_LISTEN_ADDR", "", "SKOPOS_LISTEN_ADDR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromLookup(lookupFrom(map[string]string{tc.key: tc.value}))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}
