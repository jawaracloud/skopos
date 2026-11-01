// Command seed loads the deterministic three-way comparison corpus into the
// standard OTel ClickHouse exporter schema and writes the matching OpenMetrics
// file used to build the real Prometheus reference TSDB.
//
// It is deliberately a plain command under the main module: it reuses the
// embedded TraceQL corpus package and the checked-in LogQL corpus, and uses
// only the standard library plus this repository's own packages.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	traceqlcorpus "github.com/jawaracloud/skopos/test/corpus/traceql"
)

const (
	defaultClickHouseURL = "http://127.0.0.1:18131"
	defaultDatabase      = "otel"
	defaultPassword      = "skopos-cerberus"
	schemaPath           = "deploy/helm/skopos/files/schema.sql"
	logqlCorpusPath      = "test/corpus/logql/corpus.json"
	metricsPath          = "test/compat/cerberus/metrics.json"
)

func main() {
	var (
		clickhouseURL  = flag.String("clickhouse-url", envOr("CERBERUS_COMPAT_CLICKHOUSE_URL", defaultClickHouseURL), "ClickHouse HTTP endpoint")
		clickhouseUser = flag.String("clickhouse-user", envOr("CERBERUS_COMPAT_CLICKHOUSE_USER", "default"), "ClickHouse user")
		clickhousePass = flag.String("clickhouse-password", envOr("CERBERUS_COMPAT_CLICKHOUSE_PASSWORD", defaultPassword), "ClickHouse password")
		database       = flag.String("database", envOr("CERBERUS_COMPAT_DATABASE", defaultDatabase), "ClickHouse database")
		schema         = flag.String("schema", schemaPath, "Helm schema.sql to render and apply")
		logql          = flag.String("logql-corpus", logqlCorpusPath, "LogQL corpus JSON")
		metrics        = flag.String("metrics", metricsPath, "metrics fixture JSON")
		openmetrics    = flag.String("prometheus-openmetrics", "", "write the Prometheus OpenMetrics fixture to this path")
	)
	flag.Parse()

	if err := validateDatabase(*database); err != nil {
		fatal(err)
	}
	if *openmetrics == "" {
		fatal(fmt.Errorf("--prometheus-openmetrics is required"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	seed := &seeder{url: *clickhouseURL, user: *clickhouseUser, password: *clickhousePass, database: *database}

	if err := seed.applySchema(ctx, *schema); err != nil {
		fatal(err)
	}
	if err := seed.truncate(ctx); err != nil {
		fatal(err)
	}
	if err := seed.loadLogs(ctx, *logql); err != nil {
		fatal(err)
	}
	if err := seed.loadTraces(ctx); err != nil {
		fatal(err)
	}
	fixture, err := readMetrics(*metrics)
	if err != nil {
		fatal(err)
	}
	if err := seed.loadMetrics(ctx, fixture); err != nil {
		fatal(err)
	}
	if err := writeOpenMetrics(*openmetrics, fixture); err != nil {
		fatal(err)
	}
	fmt.Printf("seed: ok: logs+traces+metrics loaded into %s; OpenMetrics written to %s\n", *database, *openmetrics)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func validateDatabase(database string) error {
	if database == "" {
		return fmt.Errorf("database must not be empty")
	}
	for _, r := range database {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return fmt.Errorf("database %q contains an invalid identifier character", database)
		}
	}
	if database[0] >= '0' && database[0] <= '9' {
		return fmt.Errorf("database %q must not start with a digit", database)
	}
	return nil
}

type seeder struct {
	url      string
	user     string
	password string
	database string
}

// storageIfBlock matches the chart's storage-enabled TTL blocks. The seeder
// uses local-only storage, so it keeps the {{- else }} branch.
var storageIfBlock = regexp.MustCompile(`(?s)\{\{- if \.Values\.clickhouse\.storage\.enabled \}\}.*?\{\{- else \}\}(.*?)\{\{- end \}\}`)

// renderChartDefaults renders the chart schema with storage disabled. It is a
// deliberately small renderer for the one conditional used by schema.sql; if
// the schema grows more conditionals, replace this with Helm or a full
// template renderer and keep the tests in sync.
func renderChartDefaults(raw, database string) string {
	// Remove the inline storage_policy fragment before matching the block form.
	// Otherwise an inline {{- if }} can pair with the next table's {{- else }}.
	rendered := strings.ReplaceAll(raw,
		"{{- if .Values.clickhouse.storage.enabled }}, storage_policy = 'tiered'{{- end }}", "")
	rendered = storageIfBlock.ReplaceAllString(rendered, "$1")
	return strings.NewReplacer(
		"{{ .Values.clickhouse.database }}", database,
		"{{ .Values.clickhouse.tables.traces }}", "otel_traces",
		"{{ .Values.clickhouse.tables.logs }}", "otel_logs",
		"{{ .Values.clickhouse.tables.metricsGauge }}", "otel_metrics_gauge",
		"{{ .Values.clickhouse.tables.metricsSum }}", "otel_metrics_sum",
		"{{ .Values.clickhouse.tables.metricsHistogram }}", "otel_metrics_histogram",
		"{{ .Values.clickhouse.tables.metricsExponentialHistogram }}", "otel_metrics_exponential_histogram",
		"{{ .Values.clickhouse.tables.metricsSummary }}", "otel_metrics_summary",
		"{{ .Values.clickhouse.ttl.tracesDays }}", "20000",
		"{{ .Values.clickhouse.ttl.logsDays }}", "20000",
		"{{ .Values.clickhouse.ttl.metricsDays }}", "20000",
		"{{ .Values.clickhouse.storage.hotDays }}", "1",
		"{{ .Values.clickhouse.storage.totalDays }}", "20000",
		"{{ .Values.clickhouse.storage.coldVolume }}", "cold",
	).Replace(rendered)
}

func (s *seeder) applySchema(ctx context.Context, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	rendered := renderChartDefaults(string(raw), s.database)
	if strings.Contains(rendered, "{{") {
		return fmt.Errorf("schema still contains Helm placeholders after rendering")
	}
	for _, statement := range splitStatements(rendered) {
		if err := s.exec(ctx, statement); err != nil {
			return fmt.Errorf("apply schema statement %.80q: %w", statement, err)
		}
	}
	return nil
}

// splitStatements strips SQL line comments and splits on semicolons outside
// quoted strings. The checked-in Helm schema has semicolons inside its header
// comments and quoted COMMENT clauses, so strings.Split is not sufficient.
func splitStatements(script string) []string {
	var (
		statements []string
		current    strings.Builder
		inSingle   bool
		inBacktick bool
	)
	runes := []rune(script)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if !inSingle && !inBacktick && r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			if i < len(runes) {
				current.WriteRune('\n')
			}
			continue
		}
		switch r {
		case '\'':
			if !inBacktick {
				if inSingle && i+1 < len(runes) && runes[i+1] == '\'' {
					current.WriteRune(r)
					current.WriteRune(runes[i+1])
					i++
					continue
				}
				inSingle = !inSingle
			}
		case '`':
			if !inSingle {
				inBacktick = !inBacktick
			}
		case ';':
			if !inSingle && !inBacktick {
				if statement := strings.TrimSpace(current.String()); statement != "" {
					statements = append(statements, statement)
				}
				current.Reset()
				continue
			}
		}
		current.WriteRune(r)
	}
	if statement := strings.TrimSpace(current.String()); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}

func (s *seeder) truncate(ctx context.Context) error {
	for _, table := range []string{
		"otel_traces",
		"otel_traces_trace_id_ts",
		"otel_logs",
		"otel_metrics_gauge",
		"otel_metrics_sum",
		"otel_metrics_histogram",
		"otel_metrics_exponential_histogram",
		"otel_metrics_summary",
	} {
		if err := s.exec(ctx, fmt.Sprintf("TRUNCATE TABLE IF EXISTS `%s`.`%s`", s.database, table)); err != nil {
			return fmt.Errorf("truncate %s: %w", table, err)
		}
	}
	return nil
}

type logCorpus struct {
	Entries []struct {
		Timestamp string            `json:"timestamp"`
		Body      string            `json:"body"`
		Labels    map[string]string `json:"labels"`
	} `json:"entries"`
}

func (s *seeder) loadLogs(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read LogQL corpus: %w", err)
	}
	var corpus logCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		return fmt.Errorf("decode LogQL corpus: %w", err)
	}
	rows := make([]string, 0, len(corpus.Entries))
	for _, entry := range corpus.Entries {
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			return fmt.Errorf("parse log timestamp %q: %w", entry.Timestamp, err)
		}
		service := entry.Labels["service_name"]
		severity := entry.Labels["severity_text"]
		// Loki stream labels are modelled as OTel resource attributes; the
		// dedicated SeverityText/ServiceName columns carry the same values, as
		// the OTel Collector ClickHouse exporter does for OTLP logs.
		row := map[string]any{
			"Timestamp":          ts.UTC().Format("2006-01-02 15:04:05.000000000"),
			"TraceId":            "",
			"SpanId":             "",
			"TraceFlags":         0,
			"SeverityText":       severity,
			"SeverityNumber":     0,
			"ServiceName":        service,
			"Body":               entry.Body,
			"ResourceSchemaUrl":  "",
			"ResourceAttributes": map[string]string{"service.name": service, "severity_text": severity},
			"ScopeSchemaUrl":     "",
			"ScopeName":          "corpus",
			"ScopeVersion":       "1.0",
			"ScopeAttributes":    map[string]string{},
			"LogAttributes":      map[string]string{},
			"EventName":          "",
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		rows = append(rows, string(encoded))
	}
	return s.insert(ctx, "otel_logs", rows)
}

func (s *seeder) loadTraces(ctx context.Context) error {
	corpus, err := traceqlcorpus.Load()
	if err != nil {
		return fmt.Errorf("load TraceQL corpus: %w", err)
	}
	rows, err := corpus.ClickHouseRows(corpus.BaseTime())
	if err != nil {
		return fmt.Errorf("render TraceQL corpus: %w", err)
	}
	return s.insert(ctx, "otel_traces", rows)
}

func (s *seeder) loadMetrics(ctx context.Context, fixture *metricsFixture) error {
	base := time.Unix(fixture.BaseTimeUnix, 0).UTC()

	gaugeRows := []string{}
	for _, series := range fixture.Gauges {
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			value := series.ValueBase + series.ValueStep*float64(i)
			gaugeRows = append(gaugeRows, marshalRow(metricRow(series.Name, series.Service, series.Attributes, "Cel", base, series.Start, offset, map[string]any{
				"Value": value,
				"Flags": 0,
			})))
		}
	}

	sumRows := []string{}
	for _, series := range fixture.Sums {
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			value := series.ValueBase + series.ValueStep*float64(i)
			sumRows = append(sumRows, marshalRow(metricRow(series.Name, series.Service, series.Attributes, "1", base, series.Start, offset, map[string]any{
				"Value":                  value,
				"Flags":                  0,
				"AggregationTemporality": 2,
				"IsMonotonic":            series.Monotonic,
			})))
		}
	}

	histRows := []string{}
	for _, series := range fixture.Histograms {
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			count := uint64(series.CountBase + i*series.CountStep)
			cumulative := make([]uint64, len(series.CumulativeFractions))
			for j, fraction := range series.CumulativeFractions {
				cumulative[j] = uint64(math.Round(fraction * float64(count)))
			}
			buckets := make([]uint64, len(cumulative))
			previous := uint64(0)
			for j, value := range cumulative {
				buckets[j] = value - previous
				previous = value
			}
			histRows = append(histRows, marshalRow(metricRow(series.Name, series.Service, series.Attributes, "s", base, series.Start, offset, map[string]any{
				"Count":                  count,
				"Sum":                    float64(count) * series.SumFactor,
				"BucketCounts":           buckets,
				"ExplicitBounds":         series.Bounds,
				"Flags":                  0,
				"Min":                    series.Min,
				"Max":                    series.Max,
				"AggregationTemporality": 2,
			})))
		}
	}

	summaryRows := []string{}
	for _, series := range fixture.Summaries {
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			count := uint64(series.CountBase + i*series.CountStep)
			quantiles := make([]float64, len(series.Quantiles))
			values := make([]float64, len(series.Quantiles))
			for j, q := range series.Quantiles {
				quantiles[j] = q.Quantile
				values[j] = q.Value
			}
			summaryRows = append(summaryRows, marshalRow(metricRow(series.Name, series.Service, series.Attributes, "s", base, series.Start, offset, map[string]any{
				"Count":                     count,
				"Sum":                       float64(count) * series.SumFactor,
				"ValueAtQuantiles.Quantile": quantiles,
				"ValueAtQuantiles.Value":    values,
				"Flags":                     0,
			})))
		}
	}

	for table, rows := range map[string][]string{
		"otel_metrics_gauge":     gaugeRows,
		"otel_metrics_sum":       sumRows,
		"otel_metrics_histogram": histRows,
		"otel_metrics_summary":   summaryRows,
	} {
		if err := s.insert(ctx, table, rows); err != nil {
			return err
		}
	}
	return nil
}

func metricRow(name, service string, attributes map[string]string, unit string, base time.Time, startOffset, offset int, extra map[string]any) map[string]any {
	row := map[string]any{
		"ResourceAttributes": map[string]string{"service.name": service},
		"ResourceSchemaUrl":  "",
		"ScopeName":          "corpus",
		"ScopeVersion":       "1.0",
		"ScopeAttributes":    map[string]string{},
		"ServiceName":        service,
		"MetricName":         name,
		"MetricDescription":  "",
		"MetricUnit":         unit,
		"Attributes":         attributes,
		"StartTimeUnix":      base.Add(time.Duration(startOffset) * time.Second).Format("2006-01-02 15:04:05"),
		"TimeUnix":           base.Add(time.Duration(offset) * time.Second).Format("2006-01-02 15:04:05"),
	}
	for key, value := range extra {
		row[key] = value
	}
	return row
}

func marshalRow(row map[string]any) string {
	encoded, err := json.Marshal(row)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func (s *seeder) insert(ctx context.Context, table string, rows []string) error {
	if len(rows) == 0 {
		return nil
	}
	body := fmt.Sprintf("INSERT INTO `%s`.`%s` FORMAT JSONEachRow\n%s\n", s.database, table, strings.Join(rows, "\n"))
	if err := s.exec(ctx, body); err != nil {
		return fmt.Errorf("insert %d rows into %s: %w", len(rows), table, err)
	}
	return nil
}

func (s *seeder) exec(ctx context.Context, sql string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewBufferString(sql))
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.user, s.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("ClickHouse HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

type metricsFixture struct {
	BaseTimeUnix int64 `json:"baseTimeUnix"`
	Gauges       []struct {
		Name       string            `json:"name"`
		Service    string            `json:"service"`
		Attributes map[string]string `json:"attributes"`
		Start      int               `json:"start"`
		End        int               `json:"end"`
		Step       int               `json:"step"`
		ValueBase  float64           `json:"valueBase"`
		ValueStep  float64           `json:"valueStep"`
	} `json:"gauges"`
	Sums []struct {
		Name       string            `json:"name"`
		Service    string            `json:"service"`
		Attributes map[string]string `json:"attributes"`
		Start      int               `json:"start"`
		End        int               `json:"end"`
		Step       int               `json:"step"`
		ValueBase  float64           `json:"valueBase"`
		ValueStep  float64           `json:"valueStep"`
		Monotonic  bool              `json:"monotonic"`
	} `json:"sums"`
	Histograms []struct {
		Name                string            `json:"name"`
		Service             string            `json:"service"`
		Attributes          map[string]string `json:"attributes"`
		Start               int               `json:"start"`
		End                 int               `json:"end"`
		Step                int               `json:"step"`
		CountBase           int               `json:"countBase"`
		CountStep           int               `json:"countStep"`
		Bounds              []float64         `json:"bounds"`
		CumulativeFractions []float64         `json:"cumulativeFractions"`
		SumFactor           float64           `json:"sumFactor"`
		Min                 float64           `json:"min"`
		Max                 float64           `json:"max"`
	} `json:"histograms"`
	Summaries []struct {
		Name       string            `json:"name"`
		Service    string            `json:"service"`
		Attributes map[string]string `json:"attributes"`
		Start      int               `json:"start"`
		End        int               `json:"end"`
		Step       int               `json:"step"`
		CountBase  int               `json:"countBase"`
		CountStep  int               `json:"countStep"`
		SumFactor  float64           `json:"sumFactor"`
		Quantiles  []struct {
			Quantile float64 `json:"quantile"`
			Value    float64 `json:"value"`
		} `json:"quantiles"`
	} `json:"summaries"`
}

func readMetrics(path string) (*metricsFixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read metrics fixture: %w", err)
	}
	var fixture metricsFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return nil, fmt.Errorf("decode metrics fixture: %w", err)
	}
	if fixture.BaseTimeUnix == 0 {
		return nil, fmt.Errorf("metrics fixture has no baseTimeUnix")
	}
	return &fixture, nil
}

// writeOpenMetrics renders the same fixture for a real Prometheus TSDB. The
// OTel histogram BucketCounts column is per-bucket; Prometheus _bucket series
// are cumulative, so this writer accumulates them.
func writeOpenMetrics(path string, fixture *metricsFixture) error {
	var out strings.Builder

	for _, series := range fixture.Gauges {
		fmt.Fprintf(&out, "# TYPE %s gauge\n", series.Name)
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			value := series.ValueBase + series.ValueStep*float64(i)
			fmt.Fprintf(&out, "%s %s %d\n", omLabels(series.Name, mergeLabels(series.Attributes, map[string]string{"service_name": series.Service})), formatFloat(value), fixture.BaseTimeUnix+int64(offset))
		}
	}
	for _, series := range fixture.Sums {
		fmt.Fprintf(&out, "# TYPE %s counter\n", series.Name)
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			value := series.ValueBase + series.ValueStep*float64(i)
			fmt.Fprintf(&out, "%s %s %d\n", omLabels(series.Name, mergeLabels(series.Attributes, map[string]string{"service_name": series.Service})), formatFloat(value), fixture.BaseTimeUnix+int64(offset))
		}
	}
	for _, series := range fixture.Histograms {
		fmt.Fprintf(&out, "# TYPE %s histogram\n", series.Name)
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			count := uint64(series.CountBase + i*series.CountStep)
			cumulative := make([]uint64, len(series.CumulativeFractions))
			for j, fraction := range series.CumulativeFractions {
				cumulative[j] = uint64(math.Round(fraction * float64(count)))
			}
			baseLabels := mergeLabels(series.Attributes, map[string]string{"service_name": series.Service})
			for j, bound := range series.Bounds {
				labels := mergeLabels(baseLabels, map[string]string{"le": formatFloat(bound)})
				fmt.Fprintf(&out, "%s %d %d\n", omLabels(series.Name+"_bucket", labels), cumulative[j], fixture.BaseTimeUnix+int64(offset))
			}
			infLabels := mergeLabels(baseLabels, map[string]string{"le": "+Inf"})
			fmt.Fprintf(&out, "%s %d %d\n", omLabels(series.Name+"_bucket", infLabels), count, fixture.BaseTimeUnix+int64(offset))
			fmt.Fprintf(&out, "%s %d %d\n", omLabels(series.Name+"_count", baseLabels), count, fixture.BaseTimeUnix+int64(offset))
			fmt.Fprintf(&out, "%s %s %d\n", omLabels(series.Name+"_sum", baseLabels), formatFloat(float64(count)*series.SumFactor), fixture.BaseTimeUnix+int64(offset))
		}
	}
	for _, series := range fixture.Summaries {
		fmt.Fprintf(&out, "# TYPE %s summary\n", series.Name)
		for i := 0; ; i++ {
			offset := series.Start + i*series.Step
			if offset > series.End {
				break
			}
			count := uint64(series.CountBase + i*series.CountStep)
			baseLabels := mergeLabels(series.Attributes, map[string]string{"service_name": series.Service})
			for _, q := range series.Quantiles {
				labels := mergeLabels(baseLabels, map[string]string{"quantile": formatFloat(q.Quantile)})
				fmt.Fprintf(&out, "%s %s %d\n", omLabels(series.Name, labels), formatFloat(q.Value), fixture.BaseTimeUnix+int64(offset))
			}
			fmt.Fprintf(&out, "%s %d %d\n", omLabels(series.Name+"_count", baseLabels), count, fixture.BaseTimeUnix+int64(offset))
			fmt.Fprintf(&out, "%s %s %d\n", omLabels(series.Name+"_sum", baseLabels), formatFloat(float64(count)*series.SumFactor), fixture.BaseTimeUnix+int64(offset))
		}
	}
	out.WriteString("# EOF\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for key, value := range a {
		out[key] = value
	}
	for key, value := range b {
		out[key] = value
	}
	return out
}

func omLabels(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	out.WriteString(name)
	if len(keys) > 0 {
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(key)
			out.WriteString("=\"")
			out.WriteString(strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(labels[key]))
			out.WriteByte('"')
		}
		out.WriteByte('}')
	}
	return out.String()
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "seed: %v\n", err)
	os.Exit(1)
}
