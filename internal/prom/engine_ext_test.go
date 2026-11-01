package prom

import (
	"context"
	"math"
	"testing"
	"time"
)

func queryVector(t *testing.T, engine *Engine, expr string) Vector {
	t.Helper()
	return queryVectorAt(t, engine, expr, atFive())
}

func queryVectorAt(t *testing.T, engine *Engine, expr string, at time.Time) Vector {
	t.Helper()
	value, err := engine.Query(context.Background(), expr, at)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	vec, ok := value.(Vector)
	if !ok {
		t.Fatalf("%s: result type %T", expr, value)
	}
	return vec
}

func valuesByLabels(vec Vector) map[string]float64 {
	out := make(map[string]float64, len(vec))
	for _, sample := range vec {
		out[labelString(sample.Labels)] = sample.V
	}
	return out
}

func TestMatrixWindowAndLookbackAreLeftOpen(t *testing.T) {
	atTen := time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC)
	engine := NewEngine(fakeFetcher{series: []Series{
		sampleSeries("window_metric", "svc", 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10),
	}})
	// [5m] at 10m covers (5m, 10m], i.e. values 6..10.
	vec := queryVectorAt(t, engine, `avg_over_time(window_metric[5m])`, atTen)
	if len(vec) != 1 || vec[0].V != 8 {
		t.Fatalf("avg_over_time = %#v, want 8", vec)
	}
	if vec := queryVectorAt(t, engine, `count_over_time(window_metric[5m])`, atTen); len(vec) != 1 || vec[0].V != 5 {
		t.Fatalf("count_over_time = %#v, want 5", vec)
	}

	// A sample exactly at eval-lookback is stale; one just inside is visible.
	boundary := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC).UnixMilli()
	engine = NewEngine(fakeFetcher{series: []Series{
		{Labels: map[string]string{"__name__": "boundary", "service_name": "svc"}, Samples: []Sample{{T: boundary, V: 1}}},
	}})
	if vec := queryVectorAt(t, engine, `boundary`, atTen); len(vec) != 0 {
		t.Fatalf("boundary sample was not stale: %#v", vec)
	}
	inside := time.Date(2026, 11, 1, 0, 5, 1, 0, time.UTC).UnixMilli()
	engine = NewEngine(fakeFetcher{series: []Series{
		{Labels: map[string]string{"__name__": "boundary", "service_name": "svc"}, Samples: []Sample{{T: inside, V: 1}}},
	}})
	if vec := queryVectorAt(t, engine, `boundary`, atTen); len(vec) != 1 {
		t.Fatalf("inside-lookback sample missing: %#v", vec)
	}
}

func TestRateCounterZeroExtrapolation(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	series := Series{
		Labels: map[string]string{"__name__": "late_counter", "service_name": "svc"},
		Samples: []Sample{
			{T: base.Add(4 * time.Minute).UnixMilli(), V: 1},
			{T: base.Add(5 * time.Minute).UnixMilli(), V: 11},
		},
	}
	engine := NewEngine(fakeFetcher{series: []Series{series}})
	vec := queryVector(t, engine, `rate(late_counter[5m])`)
	if len(vec) != 1 {
		t.Fatalf("rate = %#v", vec)
	}
	// Without zero-point extrapolation the result would be 15/300; the counter
	// zero point at 4m helps only 6s, so the increase is 11.
	want := 11.0 / 300
	if math.Abs(vec[0].V-want) > 1e-12 {
		t.Fatalf("rate = %v, want %v", vec[0].V, want)
	}
}

func setOperatorEngine() *Engine {
	base := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC).UnixMilli()
	mk := func(name string, labels map[string]string, value float64) Series {
		lset := map[string]string{"__name__": name}
		for k, v := range labels {
			lset[k] = v
		}
		return Series{Labels: lset, Samples: []Sample{{T: base, V: value}}}
	}
	return NewEngine(fakeFetcher{series: []Series{
		mk("a", map[string]string{"job": "x", "instance": "1"}, 1),
		mk("a", map[string]string{"job": "x", "instance": "2"}, 2),
		mk("a", map[string]string{"job": "y", "instance": "1"}, 3),
		mk("b", map[string]string{"job": "x"}, 10),
		mk("b", map[string]string{"job": "z"}, 20),
	}})
}

func TestSetOperators(t *testing.T) {
	engine := setOperatorEngine()
	tests := []struct {
		query string
		want  map[string]float64
	}{
		{
			`a and on(job) b`,
			map[string]float64{
				`{__name__="a",instance="1",job="x"}`: 1,
				`{__name__="a",instance="2",job="x"}`: 2,
			},
		},
		{
			`a or on(job) b`,
			map[string]float64{
				`{__name__="a",instance="1",job="x"}`: 1,
				`{__name__="a",instance="2",job="x"}`: 2,
				`{__name__="a",instance="1",job="y"}`: 3,
				`{__name__="b",job="z"}`:              20,
			},
		},
		{
			`a unless on(job) b`,
			map[string]float64{
				`{__name__="a",instance="1",job="y"}`: 3,
			},
		},
		{
			`a and ignoring(instance) b`,
			map[string]float64{
				`{__name__="a",instance="1",job="x"}`: 1,
				`{__name__="a",instance="2",job="x"}`: 2,
			},
		},
	}
	for _, tc := range tests {
		got := valuesByLabels(queryVector(t, engine, tc.query))
		if len(got) != len(tc.want) {
			t.Fatalf("%s = %#v, want %#v", tc.query, got, tc.want)
		}
		for key, want := range tc.want {
			if got[key] != want {
				t.Fatalf("%s[%s] = %v, want %v (all=%#v)", tc.query, key, got[key], want, got)
			}
		}
	}
}

func TestTopkBottomkAndCountValues(t *testing.T) {
	engine := setOperatorEngine()
	top := queryVector(t, engine, `topk(2, a)`)
	if len(top) != 2 || top[0].V != 3 || top[1].V != 2 {
		t.Fatalf("topk = %#v", top)
	}
	bottom := queryVector(t, engine, `bottomk(2, a)`)
	if len(bottom) != 2 || bottom[0].V != 1 || bottom[1].V != 2 {
		t.Fatalf("bottomk = %#v", bottom)
	}
	if got := queryVector(t, engine, `topk(0, a)`); len(got) != 0 {
		t.Fatalf("topk(0) = %#v", got)
	}
	grouped := queryVector(t, engine, `topk by (job) (1, a)`)
	if len(grouped) != 2 {
		t.Fatalf("topk by job = %#v", grouped)
	}
	if got := valuesByLabels(grouped)[`{__name__="a",instance="2",job="x"}`]; got != 2 {
		t.Fatalf("topk by job x = %#v", grouped)
	}
	if got := valuesByLabels(grouped)[`{__name__="a",instance="1",job="y"}`]; got != 3 {
		t.Fatalf("topk by job y = %#v", grouped)
	}

	counts := queryVector(t, engine, `count_values("value", a)`)
	got := valuesByLabels(counts)
	if len(got) != 3 || got[`{value="1"}`] != 1 || got[`{value="2"}`] != 1 || got[`{value="3"}`] != 1 {
		t.Fatalf("count_values = %#v", counts)
	}
}

func TestGroupLeftGroupRight(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC).UnixMilli()
	mk := func(name string, labels map[string]string, value float64) Series {
		lset := map[string]string{"__name__": name}
		for k, v := range labels {
			lset[k] = v
		}
		return Series{Labels: lset, Samples: []Sample{{T: base, V: value}}}
	}
	engine := NewEngine(fakeFetcher{series: []Series{
		mk("many", map[string]string{"job": "x", "instance": "1", "method": "GET"}, 2),
		mk("many", map[string]string{"job": "x", "instance": "2", "method": "POST"}, 3),
		mk("one", map[string]string{"job": "x", "version": "v1"}, 10),
	}})

	left := queryVector(t, engine, `many * on(job) group_left(version) one`)
	got := valuesByLabels(left)
	want := map[string]float64{
		`{instance="1",job="x",method="GET",version="v1"}`:  20,
		`{instance="2",job="x",method="POST",version="v1"}`: 30,
	}
	if len(got) != len(want) {
		t.Fatalf("group_left = %#v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("group_left[%s] = %v, want %v (all=%#v)", key, got[key], value, got)
		}
	}

	right := queryVector(t, engine, `one * on(job) group_right(version) many`)
	got = valuesByLabels(right)
	want = map[string]float64{
		`{instance="1",job="x",method="GET",version="v1"}`:  20,
		`{instance="2",job="x",method="POST",version="v1"}`: 30,
	}
	if len(got) != len(want) {
		t.Fatalf("group_right = %#v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("group_right[%s] = %v, want %v (all=%#v)", key, got[key], value, got)
		}
	}

	comparison := queryVector(t, engine, `many < on(job) group_left(version) one`)
	got = valuesByLabels(comparison)
	want = map[string]float64{
		`{__name__="many",instance="1",job="x",method="GET",version="v1"}`:  2,
		`{__name__="many",instance="2",job="x",method="POST",version="v1"}`: 3,
	}
	if len(got) != len(want) {
		t.Fatalf("group_left comparison = %#v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("group_left comparison[%s] = %v, want %v (all=%#v)", key, got[key], value, got)
		}
	}

	groupRightComparison := queryVector(t, engine, `one > on(job) group_right(version) many`)
	got = valuesByLabels(groupRightComparison)
	want = map[string]float64{
		`{__name__="many",instance="1",job="x",method="GET",version="v1"}`:  10,
		`{__name__="many",instance="2",job="x",method="POST",version="v1"}`: 10,
	}
	if len(got) != len(want) {
		t.Fatalf("group_right comparison = %#v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("group_right comparison[%s] = %v, want %v (all=%#v)", key, got[key], value, got)
		}
	}
}

func TestAbsentAndAbsentOverTime(t *testing.T) {
	engine := testEngine()
	vec := queryVector(t, engine, `absent(http_requests_total{service_name="missing",instance="x"})`)
	if len(vec) != 1 || vec[0].V != 1 {
		t.Fatalf("absent = %#v", vec)
	}
	if vec[0].Labels["service_name"] != "missing" || vec[0].Labels["instance"] != "x" {
		t.Fatalf("absent labels = %#v", vec[0].Labels)
	}
	if _, ok := vec[0].Labels["__name__"]; ok {
		t.Fatalf("absent kept __name__: %#v", vec[0].Labels)
	}
	if vec := queryVector(t, engine, `absent(http_requests_total)`); len(vec) != 0 {
		t.Fatalf("absent existing = %#v", vec)
	}
	vec = queryVector(t, engine, `absent(temperature_celsius{service_name="missing",room=~"a|b"})`)
	if len(vec) != 1 || vec[0].Labels["service_name"] != "missing" {
		t.Fatalf("absent regex labels = %#v", vec)
	}
	if _, ok := vec[0].Labels["room"]; ok {
		t.Fatalf("absent kept regex label: %#v", vec[0].Labels)
	}

	vec = queryVector(t, engine, `absent_over_time(http_requests_total{service_name="missing"}[5m])`)
	if len(vec) != 1 || vec[0].V != 1 || vec[0].Labels["service_name"] != "missing" {
		t.Fatalf("absent_over_time = %#v", vec)
	}
	if vec := queryVector(t, engine, `absent_over_time(http_requests_total[5m])`); len(vec) != 0 {
		t.Fatalf("absent_over_time existing = %#v", vec)
	}
}

func TestLabelReplaceAndLabelJoin(t *testing.T) {
	engine := testEngine()
	vec := queryVector(t, engine, `label_replace(http_requests_total{service_name="checkout"}, "service", "$1", "service_name", "(.*)")`)
	if len(vec) != 1 || vec[0].Labels["service"] != "checkout" || vec[0].Labels["__name__"] != "http_requests_total" {
		t.Fatalf("label_replace = %#v", vec)
	}
	vec = queryVector(t, engine, `label_replace(http_requests_total{service_name="checkout"}, "__name__", "renamed_$1", "__name__", "http_(.*)")`)
	if len(vec) != 1 || vec[0].Labels["__name__"] != "renamed_requests_total" {
		t.Fatalf("label_replace name = %#v", vec)
	}
	vec = queryVector(t, engine, `label_join(http_requests_total{service_name="checkout"}, "combo", "-", "service_name", "__name__")`)
	if len(vec) != 1 || vec[0].Labels["combo"] != "checkout-http_requests_total" {
		t.Fatalf("label_join = %#v", vec)
	}
	if _, err := engine.Query(context.Background(), `label_replace(label_replace(http_requests_total, "instance", "same", "instance", ".*"), "service_name", "same", "service_name", ".*")`, atFive()); err == nil {
		t.Fatal("expected duplicate label set error")
	}
}

func TestHistogramQuantilePrometheusSemantics(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC).UnixMilli()
	bucket := func(le string, count float64) Series {
		return Series{
			Labels:  map[string]string{"__name__": "x_bucket", "service_name": "svc", "le": le},
			Samples: []Sample{{T: base, V: count}},
		}
	}
	engine := NewEngine(fakeFetcher{series: []Series{
		bucket("0.1", 5), bucket("0.5", 3), bucket("1", 4), bucket("+Inf", 10),
	}})
	at := time.UnixMilli(base)
	// Non-monotonic buckets are repaired to 5,5,5,10 before interpolation.
	if vec := queryVectorAt(t, engine, `histogram_quantile(0.95, x_bucket)`, at); len(vec) != 1 || vec[0].V != 1 {
		t.Fatalf("q=0.95 = %#v, want 1", vec)
	}
	if vec := queryVectorAt(t, engine, `histogram_quantile(-0.1, x_bucket)`, at); len(vec) != 1 || !math.IsInf(vec[0].V, -1) {
		t.Fatalf("q<0 = %#v, want -Inf", vec)
	}
	if vec := queryVectorAt(t, engine, `histogram_quantile(0/0, x_bucket)`, at); len(vec) != 1 || !math.IsNaN(vec[0].V) {
		t.Fatalf("q=NaN = %#v, want NaN", vec)
	}

	missingInf := NewEngine(fakeFetcher{series: []Series{
		{Labels: map[string]string{"__name__": "x_bucket", "service_name": "svc", "le": "1"}, Samples: []Sample{{T: base, V: 3}}},
		{Labels: map[string]string{"__name__": "x_bucket", "service_name": "svc", "le": "2"}, Samples: []Sample{{T: base, V: 5}}},
	}})
	if vec := queryVectorAt(t, missingInf, `histogram_quantile(0.5, x_bucket)`, at); len(vec) != 1 || !math.IsNaN(vec[0].V) {
		t.Fatalf("missing +Inf = %#v, want NaN", vec)
	}
}

func TestRoundClampAndUnary(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC).UnixMilli()
	engine := NewEngine(fakeFetcher{series: []Series{
		{Labels: map[string]string{"__name__": "g", "service_name": "svc"}, Samples: []Sample{{T: base, V: -2.5}}},
	}})
	// Prometheus rounds ties up: -2.5 -> -2, not math.Round's -3.
	if vec := queryVector(t, engine, `round(g)`); len(vec) != 1 || vec[0].V != -2 {
		t.Fatalf("round = %#v", vec)
	}
	if vec := queryVector(t, engine, `clamp(g, 1, 0)`); len(vec) != 0 {
		t.Fatalf("clamp inverted = %#v", vec)
	}
	if vec := queryVector(t, engine, `clamp_min(g, 0)`); len(vec) != 1 || vec[0].V != 0 {
		t.Fatalf("clamp_min = %#v", vec)
	}
	if vec := queryVector(t, engine, `+g`); len(vec) != 1 || vec[0].Labels["__name__"] != "g" {
		t.Fatalf("unary plus = %#v", vec)
	}
	if vec := queryVector(t, engine, `-g`); len(vec) != 1 || vec[0].V != 2.5 {
		t.Fatalf("unary minus = %#v", vec)
	}
	if _, ok := queryVector(t, engine, `-g`)[0].Labels["__name__"]; ok {
		t.Fatal("unary minus kept metric name")
	}
}

func TestScalarLeftComparisonKeepsVectorValue(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC).UnixMilli()
	engine := NewEngine(fakeFetcher{series: []Series{
		{Labels: map[string]string{"__name__": "g", "service_name": "svc"}, Samples: []Sample{{T: base, V: 300}}},
	}})
	vec := queryVector(t, engine, `250 < g`)
	if len(vec) != 1 || vec[0].V != 300 {
		t.Fatalf("250 < g = %#v, want value 300", vec)
	}
	if vec := queryVector(t, engine, `g > 250`); len(vec) != 1 || vec[0].V != 300 {
		t.Fatalf("g > 250 = %#v", vec)
	}
	if vec := queryVector(t, engine, `250 < bool g`); len(vec) != 1 || vec[0].V != 1 {
		t.Fatalf("250 < bool g = %#v", vec)
	}
}

func TestLastOverTimeKeepsMetricName(t *testing.T) {
	engine := testEngine()
	last := queryVector(t, engine, `last_over_time(http_requests_total[5m])`)
	if len(last) == 0 || last[0].Labels["__name__"] != "http_requests_total" {
		t.Fatalf("last_over_time labels = %#v", last)
	}
	avg := queryVector(t, engine, `avg_over_time(http_requests_total[5m])`)
	if len(avg) == 0 {
		t.Fatal("avg_over_time empty")
	}
	if _, ok := avg[0].Labels["__name__"]; ok {
		t.Fatalf("avg_over_time kept name: %#v", avg[0].Labels)
	}
}
