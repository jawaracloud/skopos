package prom

import (
	"context"
	"math"
	"testing"
	"time"
)

type fakeFetcher struct {
	series []Series
}

func (f fakeFetcher) Fetch(_ context.Context, sel Selector, start, end time.Time) ([]Series, error) {
	startMs := start.UnixMilli()
	endMs := end.UnixMilli()
	out := make([]Series, 0, len(f.series))
	for _, series := range f.series {
		if !matchesAll(sel.Matchers, series.Labels) {
			continue
		}
		samples := make([]Sample, 0, len(series.Samples))
		for _, sample := range series.Samples {
			if sample.T >= startMs && sample.T <= endMs {
				samples = append(samples, sample)
			}
		}
		if len(samples) > 0 {
			out = append(out, Series{Labels: cloneLabels(series.Labels), Samples: samples})
		}
	}
	return out, nil
}

func sampleSeries(name, service string, values ...float64) Series {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	samples := make([]Sample, 0, len(values))
	for i, v := range values {
		samples = append(samples, Sample{T: base.Add(time.Duration(i) * time.Minute).UnixMilli(), V: v})
	}
	return Series{
		Labels:  map[string]string{"__name__": name, "service_name": service},
		Samples: samples,
	}
}

func testEngine() *Engine {
	fetcher := fakeFetcher{series: []Series{
		sampleSeries("http_requests_total", "checkout", 1, 2, 3, 4, 5, 6),
		sampleSeries("http_requests_total", "cart", 10, 20, 30, 40, 50, 60),
		sampleSeries("http_requests_total", "reset", 1, 2, 1, 2, 3, 4),
	}}
	e := NewEngine(fetcher)
	e.Lookback = 5 * time.Minute
	return e
}

func atFive() time.Time {
	return time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
}

func TestInstantSelector(t *testing.T) {
	v, err := testEngine().Query(context.Background(), `http_requests_total{service_name="checkout"}`, atFive())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	vec := v.(Vector)
	if len(vec) != 1 || vec[0].V != 6 {
		t.Fatalf("vector = %#v, want checkout=6", vec)
	}
}

func TestRateAndCounterReset(t *testing.T) {
	tests := []struct {
		query string
		want  float64
	}{
		{`rate(http_requests_total{service_name="checkout"}[2m])`, 2.0 / 120.0},
		{`increase(http_requests_total{service_name="checkout"}[2m])`, 2},
		{`rate(http_requests_total{service_name="reset"}[5m])`, 5.0 / 300.0},
		{`irate(http_requests_total{service_name="checkout"}[5m])`, 1.0 / 60.0},
	}
	for _, tc := range tests {
		v, err := testEngine().Query(context.Background(), tc.query, atFive())
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		vec := v.(Vector)
		if len(vec) != 1 {
			t.Fatalf("%s: vector = %#v", tc.query, vec)
		}
		if math.Abs(vec[0].V-tc.want) > 1e-9 {
			t.Fatalf("%s = %v, want %v", tc.query, vec[0].V, tc.want)
		}
	}
}

func TestAggregation(t *testing.T) {
	v, err := testEngine().Query(context.Background(), `sum by (service_name) (http_requests_total)`, atFive())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	vec := v.(Vector)
	got := map[string]float64{}
	for _, s := range vec {
		got[s.Labels["service_name"]] = s.V
	}
	want := map[string]float64{"checkout": 6, "cart": 60, "reset": 4}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("sum[%s] = %v, want %v (all=%v)", k, got[k], v, got)
		}
	}
	if _, ok := vec[0].Labels["__name__"]; ok {
		t.Fatalf("aggregation kept __name__: %#v", vec[0].Labels)
	}
}

func TestOverTimeAndOffset(t *testing.T) {
	v, err := testEngine().Query(context.Background(), `avg_over_time(http_requests_total{service_name="checkout"}[2m])`, atFive())
	if err != nil {
		t.Fatalf("avg_over_time: %v", err)
	}
	if got := v.(Vector)[0].V; got != 5 {
		t.Fatalf("avg_over_time = %v, want 5", got)
	}

	v, err = testEngine().Query(context.Background(), `http_requests_total{service_name="checkout"} offset 1m`, atFive())
	if err != nil {
		t.Fatalf("offset: %v", err)
	}
	if got := v.(Vector)[0].V; got != 5 {
		t.Fatalf("offset value = %v, want 5", got)
	}
}

func TestScalarVectorBinary(t *testing.T) {
	v, err := testEngine().Query(context.Background(), `http_requests_total{service_name="checkout"} * 2`, atFive())
	if err != nil {
		t.Fatalf("binary: %v", err)
	}
	if got := v.(Vector)[0].V; got != 12 {
		t.Fatalf("binary value = %v, want 12", got)
	}
}

func TestQueryRange(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 3, 0, 0, time.UTC)
	end := atFive()
	matrix, err := testEngine().QueryRange(context.Background(), `http_requests_total{service_name="checkout"}`, start, end, time.Minute)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(matrix) != 1 {
		t.Fatalf("matrix = %#v", matrix)
	}
	want := []float64{4, 5, 6}
	if len(matrix[0].Samples) != len(want) {
		t.Fatalf("samples = %#v", matrix[0].Samples)
	}
	for i, v := range want {
		if matrix[0].Samples[i].V != v {
			t.Fatalf("sample %d = %v, want %v", i, matrix[0].Samples[i].V, v)
		}
	}
}

func TestHistogramQuantile(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	bucket := func(le string, values ...float64) Series {
		samples := make([]Sample, len(values))
		for i, v := range values {
			samples[i] = Sample{T: base.Add(time.Duration(i) * time.Minute).UnixMilli(), V: v}
		}
		return Series{
			Labels: map[string]string{
				"__name__":     "http_request_duration_seconds_bucket",
				"service_name": "checkout",
				"le":           le,
			},
			Samples: samples,
		}
	}
	engine := NewEngine(fakeFetcher{series: []Series{
		bucket("0.1", 1, 1, 1, 1, 1, 1),
		bucket("0.5", 3, 3, 3, 3, 3, 3),
		bucket("1", 4, 4, 4, 4, 4, 4),
		bucket("+Inf", 5, 5, 5, 5, 5, 5),
	}})
	v, err := engine.Query(context.Background(), `histogram_quantile(0.5, rate(http_request_duration_seconds_bucket[5m]))`, atFive())
	if err != nil {
		t.Fatalf("histogram_quantile: %v", err)
	}
	vec := v.(Vector)
	if len(vec) != 1 {
		t.Fatalf("vector = %#v", vec)
	}
	if math.Abs(vec[0].V-0.4) > 1e-9 {
		t.Fatalf("quantile = %v, want 0.4", vec[0].V)
	}
	if _, ok := vec[0].Labels["le"]; ok {
		t.Fatal("histogram_quantile kept le")
	}
}

func TestRateExtrapolatesToRangeBoundaries(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	series := Series{
		Labels: map[string]string{"__name__": "sparse_total", "service_name": "sparse"},
		Samples: []Sample{
			{T: base.Add(time.Minute).UnixMilli(), V: 1},
			{T: base.Add(2 * time.Minute).UnixMilli(), V: 2},
		},
	}
	engine := NewEngine(fakeFetcher{series: []Series{series}})
	v, err := engine.Query(context.Background(), `rate(sparse_total[5m])`, atFive())
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	vec := v.(Vector)
	if len(vec) != 1 {
		t.Fatalf("vector = %#v", vec)
	}
	want := 1 * 2.5 / 300
	if math.Abs(vec[0].V-want) > 1e-9 {
		t.Fatalf("rate = %v, want %v", vec[0].V, want)
	}
}

func TestTimestampUsesSampleTime(t *testing.T) {
	atSix := time.Date(2026, 11, 1, 0, 6, 0, 0, time.UTC)
	v, err := testEngine().Query(context.Background(), `timestamp(http_requests_total{service_name="checkout"})`, atSix)
	if err != nil {
		t.Fatalf("timestamp: %v", err)
	}
	vec := v.(Vector)
	if len(vec) != 1 {
		t.Fatalf("vector = %#v", vec)
	}
	want := float64(time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC).Unix())
	if vec[0].V != want {
		t.Fatalf("timestamp = %v, want %v", vec[0].V, want)
	}
}

func TestMetricNameDropping(t *testing.T) {
	tests := []struct {
		query   string
		wantHas bool
	}{
		{`rate(http_requests_total{service_name="checkout"}[2m])`, false},
		{`http_requests_total{service_name="checkout"} * 2`, false},
		{`http_requests_total{service_name="checkout"} > 0`, true},
		{`http_requests_total{service_name="checkout"} > bool 0`, false},
	}
	for _, tc := range tests {
		v, err := testEngine().Query(context.Background(), tc.query, atFive())
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		vec := v.(Vector)
		if len(vec) == 0 {
			t.Fatalf("%s: empty vector", tc.query)
		}
		_, has := vec[0].Labels["__name__"]
		if has != tc.wantHas {
			t.Fatalf("%s __name__ present = %v, want %v (%#v)", tc.query, has, tc.wantHas, vec[0].Labels)
		}
	}
}

func TestHistogramQuantileInfBucket(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	bucket := func(le string, count float64) Series {
		return Series{
			Labels: map[string]string{"__name__": "x_bucket", "service_name": "checkout", "le": le},
			Samples: []Sample{
				{T: base.UnixMilli(), V: count},
				{T: base.Add(5 * time.Minute).UnixMilli(), V: count},
			},
		}
	}
	engine := NewEngine(fakeFetcher{series: []Series{
		bucket("0.1", 1),
		bucket("0.5", 2),
		bucket("1", 2),
		bucket("+Inf", 10),
	}})
	v, err := engine.Query(context.Background(), `histogram_quantile(0.99, x_bucket)`, atFive())
	if err != nil {
		t.Fatalf("histogram_quantile: %v", err)
	}
	vec := v.(Vector)
	if len(vec) != 1 || vec[0].V != 1 {
		t.Fatalf("inf bucket = %#v, want 1", vec)
	}
}

func TestLabelRegexIsAnchored(t *testing.T) {
	v, err := testEngine().Query(context.Background(), `http_requests_total{service_name=~"check"}`, atFive())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(v.(Vector)) != 0 {
		t.Fatalf("unanchored regex matched: %#v", v)
	}
}

func TestUnsupportedFunction(t *testing.T) {
	if _, err := testEngine().Query(context.Background(), `label_replace(http_requests_total, "a", "$1", "b", "(.*)")`, atFive()); err == nil {
		t.Fatal("expected unsupported function error")
	}
}
