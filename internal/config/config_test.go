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
	if cfg.Tables.Traces != "otel_traces" || cfg.Tables.MetricsSummary != "otel_metrics_summary" {
		t.Errorf("Tables = %#v", cfg.Tables)
	}
	if cfg.MaxScanRows != 200_000 || cfg.MaxQueryRange != 7*24*time.Hour {
		t.Errorf("query limits = %d/%s", cfg.MaxScanRows, cfg.MaxQueryRange)
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
	if cfg.AuthEnabled || cfg.AuthUsername != "" || cfg.AuthPassword != "" {
		t.Errorf("auth defaults = %#v", cfg)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := FromLookup(lookupFrom(map[string]string{
		"SKOPOS_LISTEN_ADDR":                   "127.0.0.1:9090",
		"SKOPOS_CLICKHOUSE_URL":                "https://clickhouse.example:8443",
		"SKOPOS_CLICKHOUSE_DATABASE":           "observability",
		"SKOPOS_CLICKHOUSE_USER":               "gateway",
		"SKOPOS_CLICKHOUSE_PASSWORD":           "secret",
		"SKOPOS_TABLE_TRACES":                  "custom_traces",
		"SKOPOS_TABLE_METRICS_SUMMARY":         "custom_summary",
		"SKOPOS_MAX_SCAN_ROWS":                 "300000",
		"SKOPOS_MAX_QUERY_RANGE":               "2h",
		"SKOPOS_CLICKHOUSE_MAX_EXECUTION_TIME": "45s",
		"SKOPOS_CLICKHOUSE_MAX_MEMORY_USAGE":   "2048",
		"SKOPOS_CLICKHOUSE_MAX_ROWS_TO_READ":   "3000",
		"SKOPOS_CLICKHOUSE_MAX_RESULT_ROWS":    "4000",
		"SKOPOS_CLICKHOUSE_MAX_RESPONSE_BYTES": "5000",
		"SKOPOS_READINESS_TIMEOUT":             "3s",
		"SKOPOS_MAX_REQUEST_BYTES":             "6000",
		"SKOPOS_LOG_LEVEL":                     "debug",
		"SKOPOS_AUTH_ENABLED":                  "true",
		"SKOPOS_AUTH_USERNAME":                 "viewer",
		"SKOPOS_AUTH_PASSWORD":                 "secret",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" || cfg.ClickHouseDatabase != "observability" ||
		cfg.ClickHouseUser != "gateway" || cfg.ClickHousePassword != "secret" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Tables.Traces != "custom_traces" || cfg.Tables.MetricsSummary != "custom_summary" {
		t.Fatalf("Tables = %#v", cfg.Tables)
	}
	if cfg.MaxScanRows != 300_000 || cfg.MaxQueryRange != 2*time.Hour {
		t.Fatalf("query limits = %d/%s", cfg.MaxScanRows, cfg.MaxQueryRange)
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
	if !cfg.AuthEnabled || cfg.AuthUsername != "viewer" || cfg.AuthPassword != "secret" {
		t.Fatalf("auth = %#v", cfg)
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
		{"table", "SKOPOS_TABLE_TRACES", "bad-name", "SKOPOS_TABLE_TRACES"},
		{"scan rows", "SKOPOS_MAX_SCAN_ROWS", "0", "SKOPOS limits"},
		{"query range", "SKOPOS_MAX_QUERY_RANGE", "soon", "SKOPOS_MAX_QUERY_RANGE"},
		{"empty listen", "SKOPOS_LISTEN_ADDR", "", "SKOPOS_LISTEN_ADDR"},
		{"mode", "SKOPOS_MODE", "backend", "SKOPOS_MODE"},
		{"query timeout", "SKOPOS_QUERY_TIMEOUT", "soon", "SKOPOS_QUERY_TIMEOUT"},
		{"zero query timeout", "SKOPOS_QUERY_TIMEOUT", "0s", "SKOPOS_QUERY_TIMEOUT"},
		{"auth bool", "SKOPOS_AUTH_ENABLED", "maybe", "SKOPOS_AUTH_ENABLED"},
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

func TestAuthRequiresCredentials(t *testing.T) {
	for _, values := range []map[string]string{
		{"SKOPOS_AUTH_ENABLED": "true"},
		{"SKOPOS_AUTH_ENABLED": "true", "SKOPOS_AUTH_USERNAME": "viewer"},
		{"SKOPOS_AUTH_ENABLED": "true", "SKOPOS_AUTH_PASSWORD": "secret"},
	} {
		if _, err := FromLookup(lookupFrom(values)); err == nil || !strings.Contains(err.Error(), "SKOPOS_AUTH") {
			t.Fatalf("values %#v error = %v", values, err)
		}
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		raw  string
		want Mode
	}{
		{"", ModeAll},
		{"all", ModeAll},
		{"ALL", ModeAll},
		{" query ", ModeQuery},
		{"query", ModeQuery},
		{"worker", ModeWorker},
		{"WORKER", ModeWorker},
		{"frontend", ModeFrontend},
	}
	for _, tc := range tests {
		got, err := ParseMode(tc.raw)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParseMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if _, err := ParseMode("backend"); err == nil || !strings.Contains(err.Error(), "SKOPOS_MODE") {
		t.Fatalf("ParseMode(backend) error = %v", err)
	}
}

func TestModeServeQueries(t *testing.T) {
	for _, mode := range []Mode{ModeAll, ModeQuery, ModeWorker} {
		if !mode.ServeQueries() {
			t.Errorf("%s should serve queries", mode)
		}
	}
	if ModeFrontend.ServeQueries() {
		t.Error("frontend must not serve queries directly")
	}
}

func TestFrontendModeDefaults(t *testing.T) {
	cfg, err := FromLookup(lookupFrom(map[string]string{
		"SKOPOS_MODE":          "frontend",
		"SKOPOS_QUERY_URLS":    "http://query:8080",
		"SKOPOS_QUERY_TIMEOUT": "12s",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if cfg.Mode != ModeFrontend {
		t.Fatalf("Mode = %q", cfg.Mode)
	}
	if len(cfg.QueryURLs) != 1 || cfg.QueryURLs[0] != "http://query:8080" {
		t.Fatalf("QueryURLs = %#v", cfg.QueryURLs)
	}
	if cfg.QueryTimeout != 12*time.Second {
		t.Fatalf("QueryTimeout = %s", cfg.QueryTimeout)
	}
}

func TestFrontendRequiresQueryURLs(t *testing.T) {
	for _, urls := range []string{"", " , , "} {
		_, err := FromLookup(lookupFrom(map[string]string{
			"SKOPOS_MODE":       "frontend",
			"SKOPOS_QUERY_URLS": urls,
		}))
		if err == nil || !strings.Contains(err.Error(), "SKOPOS_QUERY_URLS") {
			t.Fatalf("SKOPOS_QUERY_URLS=%q error = %v", urls, err)
		}
	}
}

func TestParseQueryURLs(t *testing.T) {
	got, err := ParseQueryURLs(" http://query-a:8080/ , https://query-b.example/skopos/, http://query-c:8080")
	if err != nil {
		t.Fatalf("ParseQueryURLs: %v", err)
	}
	want := []string{
		"http://query-a:8080",
		"https://query-b.example/skopos",
		"http://query-c:8080",
	}
	if len(got) != len(want) {
		t.Fatalf("ParseQueryURLs = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseQueryURLs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseQueryURLsInvalid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", "at least one URL"},
		{"relative", "/query", "http or https"},
		{"scheme", "ftp://query:8080", "http or https"},
		{"host", "http://", "no host"},
		{"fragment", "http://query:8080/#frag", "fragment"},
		{"parse", "http://[::1", "invalid URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseQueryURLs(tc.raw)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestNonFrontendIgnoresQueryURLs(t *testing.T) {
	for _, mode := range []string{"all", "query", "worker"} {
		cfg, err := FromLookup(lookupFrom(map[string]string{
			"SKOPOS_MODE":       mode,
			"SKOPOS_QUERY_URLS": "not a url",
		}))
		if err != nil {
			t.Fatalf("%s mode rejected SKOPOS_QUERY_URLS: %v", mode, err)
		}
		if len(cfg.QueryURLs) != 0 {
			t.Fatalf("%s mode QueryURLs = %#v", mode, cfg.QueryURLs)
		}
	}
}
