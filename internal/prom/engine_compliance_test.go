package prom

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

func complianceEngine(series ...Series) *Engine {
	engine := NewEngine(fakeFetcher{series: series})
	engine.Lookback = 5 * time.Minute
	return engine
}

func seriesAt(name string, extra map[string]string, values ...float64) Series {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	labels := map[string]string{"__name__": name}
	for key, value := range extra {
		labels[key] = value
	}
	samples := make([]Sample, 0, len(values))
	for i, value := range values {
		samples = append(samples, Sample{T: base.Add(time.Duration(i) * time.Minute).UnixMilli(), V: value})
	}
	return Series{Labels: labels, Samples: samples}
}

func queryVectorAtTime(t *testing.T, engine *Engine, expr string, at time.Time) Vector {
	t.Helper()
	value, err := engine.Query(context.Background(), expr, at)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	vector, ok := value.(Vector)
	if !ok {
		t.Fatalf("%s: result type %T, want Vector", expr, value)
	}
	return vector
}

func TestAtModifier(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	engine := complianceEngine(seriesAt("metric", map[string]string{"job": "1"}, 1, 2, 3, 4, 5, 6))
	at := base.Add(5 * time.Minute)
	pinned := base.Add(2 * time.Minute).UnixMilli()

	tests := []struct {
		expr string
		want float64
	}{
		{`metric @ ` + formatMillis(pinned), 3},
		{`metric @ ` + formatMillis(pinned) + ` offset 1m`, 2},
		{`metric @ start()`, 6},
		{`metric @ end()`, 6},
	}
	for _, tc := range tests {
		vector := queryVectorAtTime(t, engine, tc.expr, at)
		if len(vector) != 1 || vector[0].V != tc.want {
			t.Fatalf("%s = %#v, want %v", tc.expr, vector, tc.want)
		}
	}
}

func formatMillis(ms int64) string {
	return strconv.FormatFloat(float64(ms)/1000, 'f', -1, 64)
}

func TestAtModifierRangeUsesStartAndEnd(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	engine := complianceEngine(seriesAt("metric", nil, 1, 2, 3))
	matrix, err := engine.QueryRange(context.Background(), `metric @ start()`, base, base.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("range @ start(): %v", err)
	}
	if len(matrix) != 1 || len(matrix[0].Samples) != 3 {
		t.Fatalf("range @ start() = %#v", matrix)
	}
	for i, sample := range matrix[0].Samples {
		if sample.V != 1 {
			t.Fatalf("range @ start() sample %d = %v, want 1", i, sample.V)
		}
	}

	matrix, err = engine.QueryRange(context.Background(), `metric @ end()`, base, base.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("range @ end(): %v", err)
	}
	for i, sample := range matrix[0].Samples {
		if sample.V != 3 {
			t.Fatalf("range @ end() sample %d = %v, want 3", i, sample.V)
		}
	}
}

func TestTimestampAtEpoch(t *testing.T) {
	base := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	series := Series{
		Labels:  map[string]string{"__name__": "metric"},
		Samples: []Sample{{T: base.UnixMilli(), V: 1}},
	}
	engine := complianceEngine(series)
	vector := queryVectorAtTime(t, engine, `timestamp(metric)`, base.Add(5*time.Second))
	if len(vector) != 1 || vector[0].V != 0 {
		t.Fatalf("timestamp at epoch = %#v, want 0", vector)
	}
}

func TestAggregationPrecisionAndNaN(t *testing.T) {
	maxFloat := 9.988465674311579e307
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	data := []Series{
		seriesAt("data", map[string]string{"test": "ten", "point": "a"}, 2),
		seriesAt("data", map[string]string{"test": "ten", "point": "b"}, 8),
		seriesAt("data", map[string]string{"test": "ten", "point": "c"}, 1e100),
		seriesAt("data", map[string]string{"test": "ten", "point": "d"}, -1e100),
	}
	engine := complianceEngine(data...)
	vector := queryVectorAtTime(t, engine, `sum(data)`, base)
	if len(vector) != 1 || vector[0].V != 10 {
		t.Fatalf("sum = %#v, want 10", vector)
	}
	vector = queryVectorAtTime(t, engine, `avg(data)`, base)
	if len(vector) != 1 || vector[0].V != 2.5 {
		t.Fatalf("avg = %#v, want 2.5", vector)
	}

	big := []Series{
		seriesAt("big", map[string]string{"point": "a"}, maxFloat),
		seriesAt("big", map[string]string{"point": "b"}, maxFloat),
		seriesAt("big", map[string]string{"point": "c"}, maxFloat),
		seriesAt("big", map[string]string{"point": "d"}, maxFloat),
	}
	vector = queryVectorAtTime(t, complianceEngine(big...), `avg(big)`, base)
	if len(vector) != 1 || vector[0].V != maxFloat {
		t.Fatalf("avg overflow = %#v, want %v", vector, maxFloat)
	}

	mixed := []Series{
		seriesAt("mixed", map[string]string{"point": "a"}, 1),
		seriesAt("mixed", map[string]string{"point": "b"}, math.NaN()),
		seriesAt("mixed", map[string]string{"point": "c"}, 4),
	}
	engine = complianceEngine(mixed...)
	vector = queryVectorAtTime(t, engine, `max(mixed)`, base)
	if len(vector) != 1 || vector[0].V != 4 {
		t.Fatalf("max with NaN = %#v, want 4", vector)
	}
	vector = queryVectorAtTime(t, engine, `min(mixed)`, base)
	if len(vector) != 1 || vector[0].V != 1 {
		t.Fatalf("min with NaN = %#v, want 1", vector)
	}

	// An arithmetic sequence has an exactly representable population
	// variance; the compensated Welford path must produce it without
	// cancellation residue.
	steady := []Series{
		seriesAt("steady", map[string]string{"point": "a"}, 1.33),
		seriesAt("steady", map[string]string{"point": "b"}, 2.66),
		seriesAt("steady", map[string]string{"point": "c"}, 3.99),
		seriesAt("steady", map[string]string{"point": "d"}, 5.32),
	}
	engine = complianceEngine(steady...)
	vector = queryVectorAtTime(t, engine, `stdvar(steady)`, base)
	if len(vector) != 1 || vector[0].V != 2.211125 {
		t.Fatalf("stdvar = %#v, want 2.211125", vector)
	}
}

func TestTopkNaNParameterIsError(t *testing.T) {
	_, err := testEngine().Query(context.Background(), `topk(NaN, http_requests_total)`, atFive())
	if err == nil || !strings.Contains(err.Error(), "Parameter value is NaN") {
		t.Fatalf("topk(NaN) error = %v", err)
	}
}

func TestLabelReplaceAllowsArbitrarySource(t *testing.T) {
	engine := complianceEngine(seriesAt("testmetric", map[string]string{"src": "source-value-10"}, 0))
	vector := queryVectorAtTime(t, engine, `label_replace(testmetric, "dst", "value-$1", "nonexistent-src", "(.*)")`, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if len(vector) != 1 || vector[0].Labels["dst"] != "value-" {
		t.Fatalf("label_replace = %#v", vector)
	}

	vector = queryVectorAtTime(t, engine, `label_replace(testmetric, "dst", "", "src", ".*")`, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if _, ok := vector[0].Labels["dst"]; ok {
		t.Fatalf("label_replace kept empty dst: %#v", vector[0].Labels)
	}
}

func TestLabelJoinWithoutSourcesAndEmptyDestination(t *testing.T) {
	engine := complianceEngine(seriesAt("testmetric", map[string]string{"src": "a"}, 0))
	vector := queryVectorAtTime(t, engine, `label_join(testmetric, "dst", ", ")`, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if len(vector) != 1 {
		t.Fatalf("label_join = %#v", vector)
	}
	if _, ok := vector[0].Labels["dst"]; ok {
		t.Fatalf("label_join kept empty dst: %#v", vector[0].Labels)
	}
}

func TestDuplicateLabelsetErrors(t *testing.T) {
	engine := complianceEngine(
		seriesAt("testmetric1", map[string]string{"src": "a"}, 0),
		seriesAt("testmetric2", map[string]string{"src": "a"}, 1),
	)
	if _, err := engine.Query(context.Background(), `ceil({__name__=~"testmetric1|testmetric2"})`, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("expected duplicate labelset error")
	}
}
