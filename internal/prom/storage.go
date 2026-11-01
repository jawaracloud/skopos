package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	promlabels "github.com/prometheus/prometheus/model/labels"

	"github.com/jawaracloud/skopos/internal/chclient"
	"github.com/jawaracloud/skopos/internal/plan"
	"github.com/jawaracloud/skopos/internal/sqlgen"
)

// ClickHouseFetcher loads gauge, sum, and histogram samples from ClickHouse.
//
// Metric and label names are normalized to Prometheus names by replacing
// characters outside [A-Za-z0-9_] with '_'. The metric unit is not appended
// yet; this is documented in docs/promql-support.md.
type ClickHouseFetcher struct {
	Client  *chclient.Client
	Schema  *plan.Schema
	Emitter *sqlgen.Emitter
	Names   plan.TableNames
	MaxRows int
}

// NewClickHouseFetcher builds a fetcher and validates its table names.
func NewClickHouseFetcher(client *chclient.Client, schema *plan.Schema, names plan.TableNames, maxRows int) (*ClickHouseFetcher, error) {
	if client == nil {
		return nil, fmt.Errorf("prom: clickhouse client is required")
	}
	if schema == nil {
		return nil, fmt.Errorf("prom: schema is required")
	}
	if maxRows <= 0 {
		return nil, fmt.Errorf("prom: max rows must be positive")
	}
	for _, name := range []string{names.MetricsGauge, names.MetricsSum, names.MetricsHistogram, names.MetricsSummary, names.MetricsExponentialHistogram} {
		if _, ok := schema.Table(name); !ok {
			return nil, fmt.Errorf("prom: schema has no table %q", name)
		}
	}
	emitter, err := sqlgen.New(schema, sqlgen.Limits{MaxLimit: maxRows, MaxRange: 7 * 24 * time.Hour})
	if err != nil {
		return nil, err
	}
	return &ClickHouseFetcher{Client: client, Schema: schema, Emitter: emitter, Names: names, MaxRows: maxRows}, nil
}

// Fetch implements Fetcher.
func (f *ClickHouseFetcher) Fetch(ctx context.Context, sel Selector, start, end time.Time) ([]Series, error) {
	gaugeFilter, sumFilter, histFilter, summaryFilter := nameFilters(sel)
	series := make(map[string]*Series)
	if err := f.fetchTable(ctx, f.Names.MetricsGauge, tableGauge, gaugeFilter, start, end, series); err != nil {
		return nil, err
	}
	if err := f.fetchTable(ctx, f.Names.MetricsSum, tableSum, sumFilter, start, end, series); err != nil {
		return nil, err
	}
	if err := f.fetchTable(ctx, f.Names.MetricsHistogram, tableHistogram, histFilter, start, end, series); err != nil {
		return nil, err
	}
	if err := f.fetchTable(ctx, f.Names.MetricsSummary, tableSummary, summaryFilter, start, end, series); err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(series))
	for key, s := range series {
		if !matchesAll(sel.Matchers, s.Labels) {
			delete(series, key)
			continue
		}
		sort.Slice(s.Samples, func(i, j int) bool { return s.Samples[i].T < s.Samples[j].T })
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	if len(out) == 0 && hasMetricNameMatcher(sel) {
		if err := f.probeExponentialHistogram(ctx, sel, start, end); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func hasMetricNameMatcher(sel Selector) bool {
	exact, regex := metricNameMatchers(sel)
	return exact != "" || regex != ""
}

type tableKind int

const (
	tableGauge tableKind = iota
	tableSum
	tableHistogram
	tableSummary
)

func (f *ClickHouseFetcher) fetchTable(ctx context.Context, table string, kind tableKind, filter plan.Expr, start, end time.Time, out map[string]*Series) error {
	selects := []plan.SelectItem{
		{Expr: plan.Col("MetricName")},
		{Expr: plan.Col("TimeUnix")},
		{Expr: plan.Col("ServiceName")},
		{Expr: plan.Col("Attributes")},
	}
	switch kind {
	case tableGauge, tableSum:
		selects = append(selects, plan.SelectItem{Expr: plan.Col("Value")})
	case tableHistogram:
		selects = append(selects,
			plan.SelectItem{Expr: plan.Col("Count")},
			plan.SelectItem{Expr: plan.Col("Sum")},
			plan.SelectItem{Expr: plan.Col("BucketCounts")},
			plan.SelectItem{Expr: plan.Col("ExplicitBounds")},
		)
	case tableSummary:
		selects = append(selects,
			plan.SelectItem{Expr: plan.Col("Count")},
			plan.SelectItem{Expr: plan.Col("Sum")},
			plan.SelectItem{Expr: plan.Col("ValueAtQuantiles.Quantile")},
			plan.SelectItem{Expr: plan.Col("ValueAtQuantiles.Value")},
		)
	}
	query := plan.Query{
		Signal:     plan.SignalMetrics,
		Table:      table,
		Select:     selects,
		Where:      filter,
		TimeColumn: "TimeUnix",
		TimeRange: &plan.TimeRange{
			Start:        start,
			End:          end,
			EndInclusive: true,
		},
		Limit: f.MaxRows,
	}
	emitted, err := f.Emitter.Emit(query)
	if err != nil {
		return fmt.Errorf("prom: build %s query: %w", table, err)
	}
	result, err := f.Client.Query(ctx, emitted.SQL, emitted.Params)
	if err != nil {
		return fmt.Errorf("prom: query %s: %w", table, err)
	}
	if result.Len() >= f.MaxRows {
		return fmt.Errorf("prom: query reached the %d-row scan limit; narrow the time range or metric selector", f.MaxRows)
	}
	for i := 0; i < result.Len(); i++ {
		switch kind {
		case tableGauge, tableSum:
			f.addGaugeRow(result, i, out)
		case tableHistogram:
			if err := f.addHistogramRow(result, i, out); err != nil {
				return err
			}
		case tableSummary:
			if err := f.addSummaryRow(result, i, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *ClickHouseFetcher) addGaugeRow(result *chclient.Result, row int, out map[string]*Series) {
	metric, _ := result.String(row, "MetricName")
	service, _ := result.String(row, "ServiceName")
	value, ok := result.Float64(row, "Value")
	if !ok {
		return
	}
	ts, err := resultTime(result, row)
	if err != nil {
		return
	}
	labels := baseLabels(metric, service, attributes(result, row))
	addSample(out, labels, Sample{T: ts, V: value})
}

func (f *ClickHouseFetcher) addHistogramRow(result *chclient.Result, row int, out map[string]*Series) error {
	metric, _ := result.String(row, "MetricName")
	service, _ := result.String(row, "ServiceName")
	ts, err := resultTime(result, row)
	if err != nil {
		return nil
	}
	base := baseLabels(metric, service, attributes(result, row))
	name := base["__name__"]
	counts, err := floatSlice(result.Rows[row]["BucketCounts"])
	if err != nil {
		return fmt.Errorf("prom: decode BucketCounts: %w", err)
	}
	bounds, err := floatSlice(result.Rows[row]["ExplicitBounds"])
	if err != nil {
		return fmt.Errorf("prom: decode ExplicitBounds: %w", err)
	}
	cumulative := 0.0
	for i, bound := range bounds {
		if i < len(counts) {
			cumulative += counts[i]
		}
		labels := cloneLabels(base)
		labels["__name__"] = name + "_bucket"
		labels["le"] = formatBound(bound)
		addSample(out, labels, Sample{T: ts, V: cumulative})
	}
	if len(counts) > len(bounds) {
		cumulative += counts[len(bounds)]
	}
	labels := cloneLabels(base)
	labels["__name__"] = name + "_bucket"
	labels["le"] = "+Inf"
	addSample(out, labels, Sample{T: ts, V: cumulative})

	if count, ok := result.Float64(row, "Count"); ok {
		labels := cloneLabels(base)
		labels["__name__"] = name + "_count"
		addSample(out, labels, Sample{T: ts, V: count})
	}
	if sum, ok := result.Float64(row, "Sum"); ok {
		labels := cloneLabels(base)
		labels["__name__"] = name + "_sum"
		addSample(out, labels, Sample{T: ts, V: sum})
	}
	return nil
}

func (f *ClickHouseFetcher) addSummaryRow(result *chclient.Result, row int, out map[string]*Series) error {
	metric, _ := result.String(row, "MetricName")
	service, _ := result.String(row, "ServiceName")
	ts, err := resultTime(result, row)
	if err != nil {
		return nil
	}
	base := baseLabels(metric, service, attributes(result, row))
	name := base["__name__"]

	if count, ok := result.Float64(row, "Count"); ok {
		labels := cloneLabels(base)
		labels["__name__"] = name + "_count"
		addSample(out, labels, Sample{T: ts, V: count})
	}
	if sum, ok := result.Float64(row, "Sum"); ok {
		labels := cloneLabels(base)
		labels["__name__"] = name + "_sum"
		addSample(out, labels, Sample{T: ts, V: sum})
	}
	quantiles, err := floatSlice(result.Rows[row]["ValueAtQuantiles.Quantile"])
	if err != nil {
		return fmt.Errorf("prom: decode ValueAtQuantiles.Quantile: %w", err)
	}
	values, err := floatSlice(result.Rows[row]["ValueAtQuantiles.Value"])
	if err != nil {
		return fmt.Errorf("prom: decode ValueAtQuantiles.Value: %w", err)
	}
	for i, quantile := range quantiles {
		if i >= len(values) {
			break
		}
		labels := cloneLabels(base)
		labels["quantile"] = formatBound(quantile)
		addSample(out, labels, Sample{T: ts, V: values[i]})
	}
	return nil
}

// probeExponentialHistogram returns an explicit error if a selector that
// produced no supported samples names an OTel exponential histogram. The probe
// is bounded and only runs for selectors with a metric-name matcher.
func (f *ClickHouseFetcher) probeExponentialHistogram(ctx context.Context, sel Selector, start, end time.Time) error {
	filter := metricNameFilter(sel)
	limit := 1000
	if f.MaxRows > 0 && f.MaxRows < limit {
		limit = f.MaxRows
	}
	query := plan.Query{
		Signal: plan.SignalMetrics,
		Table:  f.Names.MetricsExponentialHistogram,
		Select: []plan.SelectItem{
			{Expr: plan.Col("MetricName")},
			{Expr: plan.Col("ServiceName")},
			{Expr: plan.Col("Attributes")},
		},
		Where:      filter,
		TimeColumn: "TimeUnix",
		TimeRange: &plan.TimeRange{
			Start:        start,
			End:          end,
			EndInclusive: true,
		},
		Limit: limit,
	}
	emitted, err := f.Emitter.Emit(query)
	if err != nil {
		return fmt.Errorf("prom: build exponential histogram probe: %w", err)
	}
	result, err := f.Client.Query(ctx, emitted.SQL, emitted.Params)
	if err != nil {
		return fmt.Errorf("prom: exponential histogram probe: %w", err)
	}
	for i := 0; i < result.Len(); i++ {
		name, _ := result.String(i, "MetricName")
		service, _ := result.String(i, "ServiceName")
		labels := baseLabels(name, service, attributes(result, i))
		family := labels["__name__"]
		candidates := []string{family}
		if !strings.HasSuffix(family, "_count") && !strings.HasSuffix(family, "_sum") {
			candidates = append(candidates, family+"_count", family+"_sum")
		}
		for _, candidate := range candidates {
			labels["__name__"] = candidate
			if matchesAll(sel.Matchers, labels) {
				return fmt.Errorf("%w: metric %q is an OpenTelemetry exponential histogram; exponential histograms and native histogram functions are not supported", ErrBadQuery, candidate)
			}
		}
	}
	return nil
}
func nameFilters(sel Selector) (gauge, sum, histogram, summary plan.Expr) {
	exact, regex := metricNameMatchers(sel)
	normalized := normalizedMetricName()
	if exact != "" {
		name := NormalizeLabelName(exact)
		base := stripHistogramSuffix(name)
		gauge = plan.Bin("=", normalized, plan.String("metric_filter", name))
		sum = plan.Bin("=", normalized, plan.String("metric_filter", name))
		histogram = plan.Bin("=", normalized, plan.String("metric_filter", base))
		summary = plan.Bin("=", normalized, plan.String("metric_filter", base))
		return gauge, sum, histogram, summary
	}
	if regex != "" {
		gauge = plan.F("match", normalized, plan.String("metric_regex_filter", regex))
		sum = plan.F("match", normalized, plan.String("metric_regex_filter", regex))
		// Histogram bucket and summary quantile selectors usually include a
		// suffix while the tables store the base name. Do not apply a name
		// filter here; the Go matcher filters the expanded series.
		return gauge, sum, nil, nil
	}
	return nil, nil, nil, nil
}

func metricNameFilter(sel Selector) plan.Expr {
	exact, regex := metricNameMatchers(sel)
	normalized := normalizedMetricName()
	if exact != "" {
		return plan.Bin("=", normalized, plan.String("metric_filter", stripHistogramSuffix(NormalizeLabelName(exact))))
	}
	if regex != "" {
		return plan.F("match", normalized, plan.String("metric_regex_filter", regex))
	}
	return nil
}

func metricNameMatchers(sel Selector) (exact, regex string) {
	for _, m := range sel.Matchers {
		if m.Name != promlabels.MetricName {
			continue
		}
		switch m.Type {
		case promlabels.MatchEqual:
			if exact == "" {
				exact = m.Value
			}
		case promlabels.MatchRegexp:
			if regex == "" {
				regex = m.Value
			}
		}
	}
	return exact, regex
}

func normalizedMetricName() plan.Expr {
	return plan.F("replaceRegexpAll",
		plan.Col("MetricName"),
		plan.String("norm_pattern", `[^A-Za-z0-9_]`),
		plan.String("norm_replacement", "_"),
	)
}

func stripHistogramSuffix(name string) string {
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func baseLabels(metric, service string, attrs map[string]string) map[string]string {
	labels := map[string]string{"__name__": NormalizeLabelName(metric)}
	if service != "" {
		labels["service_name"] = service
	}
	for k, v := range attrs {
		name := NormalizeLabelName(k)
		if name == "__name__" || name == "" {
			continue
		}
		if _, exists := labels[name]; exists {
			continue
		}
		labels[name] = v
	}
	return labels
}

func addSample(out map[string]*Series, labels map[string]string, sample Sample) {
	key := labelsKey(labels)
	s := out[key]
	if s == nil {
		s = &Series{Labels: cloneLabels(labels)}
		out[key] = s
	}
	s.Samples = append(s.Samples, sample)
}

func attributes(result *chclient.Result, row int) map[string]string {
	v, ok := result.Rows[row]["Attributes"]
	if !ok {
		return nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func resultTime(result *chclient.Result, row int) (int64, error) {
	s, ok := result.String(row, "TimeUnix")
	if !ok {
		return 0, fmt.Errorf("missing TimeUnix")
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("invalid TimeUnix %q", s)
}

func floatSlice(v any) ([]float64, error) {
	switch values := v.(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]float64, 0, len(values))
		for _, value := range values {
			f, err := toFloat(value)
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		return out, nil
	case []float64:
		return values, nil
	default:
		return nil, fmt.Errorf("unexpected array type %T", v)
	}
}

func toFloat(v any) (float64, error) {
	switch value := v.(type) {
	case json.Number:
		return value.Float64()
	case float64:
		return value, nil
	case float32:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case int:
		return float64(value), nil
	case string:
		return strconv.ParseFloat(value, 64)
	default:
		return 0, fmt.Errorf("unexpected number type %T", v)
	}
}

func formatBound(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func matchesAll(matchers []*promlabels.Matcher, labels map[string]string) bool {
	for _, m := range matchers {
		if !m.Matches(labels[m.Name]) {
			return false
		}
	}
	return true
}

// Metadata is the Prometheus metadata record for one metric name.
type Metadata struct {
	Type string
	Help string
	Unit string
}

// Metadata returns metric metadata from the gauge, sum, and histogram tables.
func (f *ClickHouseFetcher) Metadata(ctx context.Context, metric string) (map[string]Metadata, error) {
	out := make(map[string]Metadata)
	tables := []struct {
		name     string
		kind     tableKind
		typeName string
	}{
		{f.Names.MetricsGauge, tableGauge, "gauge"},
		{f.Names.MetricsSum, tableSum, "counter"},
		{f.Names.MetricsHistogram, tableHistogram, "histogram"},
		{f.Names.MetricsSummary, tableSummary, "summary"},
	}
	for _, table := range tables {
		var filter plan.Expr
		if metric != "" {
			filter = plan.Bin("=",
				plan.F("replaceRegexpAll",
					plan.Col("MetricName"),
					plan.String("norm_pattern", `[^A-Za-z0-9_]`),
					plan.String("norm_replacement", "_"),
				),
				plan.String("metric_filter", NormalizeLabelName(metric)),
			)
		}
		query := plan.Query{
			Signal: plan.SignalMetrics,
			Table:  table.name,
			Select: []plan.SelectItem{
				{Expr: plan.Col("MetricName")},
				{Expr: plan.Agg("any", plan.Col("MetricDescription")), Alias: "help"},
				{Expr: plan.Agg("any", plan.Col("MetricUnit")), Alias: "unit"},
			},
			Where:   filter,
			GroupBy: []string{"MetricName"},
			Limit:   f.MaxRows,
		}
		emitted, err := f.Emitter.Emit(query)
		if err != nil {
			return nil, fmt.Errorf("prom: build metadata query: %w", err)
		}
		result, err := f.Client.Query(ctx, emitted.SQL, emitted.Params)
		if err != nil {
			return nil, fmt.Errorf("prom: metadata query %s: %w", table.name, err)
		}
		if result.Len() >= f.MaxRows {
			return nil, fmt.Errorf("prom: metadata query reached the %d-row limit", f.MaxRows)
		}
		for i := 0; i < result.Len(); i++ {
			name, _ := result.String(i, "MetricName")
			help, _ := result.String(i, "help")
			unit, _ := result.String(i, "unit")
			out[NormalizeLabelName(name)] = Metadata{Type: table.typeName, Help: help, Unit: unit}
		}
	}
	return out, nil
}
