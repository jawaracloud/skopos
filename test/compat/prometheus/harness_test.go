package promcompat

import (
	"context"
	"fmt"
	"testing"
	"time"
)

var (
	baseTime = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	evalTime = baseTime.Add(10 * time.Minute)
)

func minutes(count int) []time.Time {
	out := make([]time.Time, count)
	for i := range out {
		out[i] = baseTime.Add(time.Duration(i) * time.Minute)
	}
	return out
}

func steady(name string, extra map[string]string, start, step float64) Series {
	times := minutes(11)
	samples := make([]Sample, len(times))
	for i, at := range times {
		samples[i] = Sample{T: at.UnixMilli(), V: start + step*float64(i)}
	}
	lset := map[string]string{"__name__": name}
	for key, value := range extra {
		lset[key] = value
	}
	return Series{Labels: lset, Samples: samples}
}

func explicit(name string, extra map[string]string, samples ...Sample) Series {
	lset := map[string]string{"__name__": name}
	for key, value := range extra {
		lset[key] = value
	}
	return Series{Labels: lset, Samples: samples}
}

func sampleAt(minute int, value float64) Sample {
	return Sample{T: baseTime.Add(time.Duration(minute) * time.Minute).UnixMilli(), V: value}
}

func bucket(name, service, le string, start, step float64) Series {
	return steady(name, map[string]string{"service_name": service, "le": le}, start, step)
}

func corpusData() []Series {
	return []Series{
		steady("http_requests_total", map[string]string{"service_name": "checkout", "instance": "0", "method": "GET"}, 100, 10),
		steady("http_requests_total", map[string]string{"service_name": "checkout", "instance": "1", "method": "POST"}, 200, 20),
		explicit("http_requests_total", map[string]string{"service_name": "cart", "instance": "0", "method": "GET"},
			sampleAt(0, 0), sampleAt(1, 10), sampleAt(2, 20), sampleAt(3, 30),
			sampleAt(4, 5), sampleAt(5, 15), sampleAt(6, 25), sampleAt(7, 35),
			sampleAt(8, 45), sampleAt(9, 55), sampleAt(10, 65)),
		steady("temperature_celsius", map[string]string{"service_name": "checkout", "instance": "0", "room": "a"}, 20, 0.5),
		steady("temperature_celsius", map[string]string{"service_name": "cart", "instance": "0", "room": "b"}, 30, -0.25),
		steady("request_info", map[string]string{"service_name": "checkout", "version": "v1", "team": "payments"}, 1, 0),
		steady("request_info", map[string]string{"service_name": "cart", "version": "v2", "team": "cart"}, 1, 0),
		explicit("sparse_total", map[string]string{"service_name": "sparse"},
			sampleAt(2, 10), sampleAt(6, 20), sampleAt(10, 30)),
		explicit("one_sample_total", map[string]string{"service_name": "one"}, sampleAt(9, 7)),
		explicit("nan_gauge", map[string]string{"service_name": "nan"},
			sampleAt(6, 1), sampleAt(7, nan()), sampleAt(8, 3)),
		explicit("nan_first_gauge", map[string]string{"service_name": "nan"},
			sampleAt(8, nan()), sampleAt(9, 2), sampleAt(10, 4)),
		explicit("single_sample_gauge", map[string]string{"service_name": "single"}, sampleAt(5, 1)),
		explicit("zero_counter", map[string]string{"service_name": "zero"},
			sampleAt(5, 0), sampleAt(6, 0), sampleAt(7, 0), sampleAt(8, 0), sampleAt(9, 0), sampleAt(10, 0)),
		explicit("lookback_gauge", map[string]string{"service_name": "boundary"}, sampleAt(5, 42)),
		explicit("hist_missing_inf_bucket", map[string]string{"service_name": "missing", "le": "1"}, sampleAt(10, 3)),
		explicit("hist_missing_inf_bucket", map[string]string{"service_name": "missing", "le": "2"}, sampleAt(10, 5)),
		explicit("hist_nonmonotonic_bucket", map[string]string{"service_name": "broken", "le": "0.1"}, sampleAt(10, 5)),
		explicit("hist_nonmonotonic_bucket", map[string]string{"service_name": "broken", "le": "0.5"}, sampleAt(10, 3)),
		explicit("hist_nonmonotonic_bucket", map[string]string{"service_name": "broken", "le": "1"}, sampleAt(10, 4)),
		explicit("hist_nonmonotonic_bucket", map[string]string{"service_name": "broken", "le": "+Inf"}, sampleAt(10, 10)),
		bucket("http_request_duration_seconds_bucket", "checkout", "0.1", 1, 0.1),
		bucket("http_request_duration_seconds_bucket", "checkout", "0.5", 3, 0.2),
		bucket("http_request_duration_seconds_bucket", "checkout", "1", 4, 0.3),
		bucket("http_request_duration_seconds_bucket", "checkout", "+Inf", 5, 0.5),
		bucket("http_request_duration_seconds_bucket", "cart", "0.1", 2, 0.1),
		bucket("http_request_duration_seconds_bucket", "cart", "0.5", 4, 0.2),
		bucket("http_request_duration_seconds_bucket", "cart", "1", 6, 0.3),
		bucket("http_request_duration_seconds_bucket", "cart", "+Inf", 8, 0.5),
		steady("http_request_duration_seconds_count", map[string]string{"service_name": "checkout"}, 5, 0.5),
		steady("http_request_duration_seconds_sum", map[string]string{"service_name": "checkout"}, 1, 0.25),
	}
}

func nan() float64 {
	return 0.0 / zero()
}

func zero() float64 { return 0 }

func instantCorpus() []string {
	return []string{
		// Selectors and offsets.
		`http_requests_total`,
		`http_requests_total{service_name="checkout"}`,
		`http_requests_total{service_name!="checkout"}`,
		`http_requests_total{instance=~"0|1"}`,
		`http_requests_total{instance!~"1"}`,
		`{__name__="temperature_celsius"}`,
		`{__name__=~"temperature_.*"}`,
		`http_requests_total offset 2m`,
		`http_requests_total offset 5m`,
		`temperature_celsius offset 5m`,

		// Counter functions.
		`rate(http_requests_total[2m])`,
		`rate(http_requests_total[5m])`,
		`rate(http_requests_total{service_name="cart"}[5m])`,
		`increase(http_requests_total[5m])`,
		`increase(http_requests_total{service_name="cart"}[5m])`,
		`irate(http_requests_total[5m])`,
		`irate(http_requests_total{service_name="cart"}[5m])`,
		`rate(sparse_total[5m])`,
		`increase(sparse_total[5m])`,
		`rate(sparse_total[10m])`,
		`rate(one_sample_total[5m])`,

		// Range aggregations.
		`avg_over_time(http_requests_total{service_name="checkout"}[5m])`,
		`avg_over_time(temperature_celsius[5m])`,
		`min_over_time(temperature_celsius[5m])`,
		`max_over_time(temperature_celsius[5m])`,
		`min_over_time(nan_gauge[5m])`,
		`max_over_time(nan_gauge[5m])`,
		`min_over_time(nan_first_gauge[5m])`,
		`max_over_time(nan_first_gauge[5m])`,
		`avg_over_time(nan_first_gauge[5m])`,
		`quantile_over_time(0.5, nan_first_gauge[5m])`,
		`count_over_time(zero_counter[5m])`,
		`rate(zero_counter[5m])`,
		`increase(zero_counter[5m])`,
		`rate(single_sample_gauge[5m])`,
		`lookback_gauge`,
		`lookback_gauge offset 5m`,
		`histogram_quantile(0.5, hist_missing_inf_bucket)`,
		`histogram_quantile(0.5, hist_nonmonotonic_bucket)`,
		`histogram_quantile(0.95, hist_nonmonotonic_bucket)`,
		`topk(2, nan_first_gauge)`,
		`bottomk(2, nan_first_gauge)`,
		`sort(nan_first_gauge)`,
		`sort_desc(nan_first_gauge)`,
		`sum_over_time(http_requests_total{service_name="checkout"}[5m])`,
		`count_over_time(http_requests_total[5m])`,
		`count_over_time(one_sample_total[5m])`,
		`last_over_time(http_requests_total[5m])`,
		`last_over_time(nan_gauge[5m])`,
		`stddev_over_time(temperature_celsius[5m])`,
		`stdvar_over_time(temperature_celsius[5m])`,
		`present_over_time(http_requests_total[5m])`,
		`quantile_over_time(0.5, temperature_celsius[5m])`,
		`quantile_over_time(0.9, http_requests_total[5m])`,
		`quantile_over_time(1.2, temperature_celsius[5m])`,
		`quantile_over_time(-0.1, temperature_celsius[5m])`,

		// Histogram quantile.
		`histogram_quantile(0.5, rate(http_request_duration_seconds_bucket[5m]))`,
		`histogram_quantile(0.9, rate(http_request_duration_seconds_bucket[5m]))`,
		`histogram_quantile(0, rate(http_request_duration_seconds_bucket[5m]))`,
		`histogram_quantile(1, rate(http_request_duration_seconds_bucket[5m]))`,
		`histogram_quantile(1.2, rate(http_request_duration_seconds_bucket[5m]))`,
		`histogram_quantile(-0.1, rate(http_request_duration_seconds_bucket[5m]))`,
		`histogram_quantile(0/0, rate(http_request_duration_seconds_bucket[5m]))`,
		`histogram_quantile(0.5, http_request_duration_seconds_bucket)`,
		`histogram_quantile(0.9, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
		`http_request_duration_seconds_count`,
		`http_request_duration_seconds_sum`,
		`rate(http_request_duration_seconds_count[5m])`,
		`increase(http_request_duration_seconds_sum[5m])`,

		// Math.
		`abs(temperature_celsius)`,
		`ceil(temperature_celsius)`,
		`floor(temperature_celsius)`,
		`sqrt(temperature_celsius)`,
		`exp(temperature_celsius)`,
		`ln(temperature_celsius)`,
		`log2(temperature_celsius)`,
		`log10(temperature_celsius)`,
		`round(temperature_celsius)`,
		`round(temperature_celsius, 0.5)`,
		`round(-temperature_celsius, 0.5)`,
		`clamp(temperature_celsius, 22, 25)`,
		`clamp(temperature_celsius, 25, 22)`,
		`clamp_min(temperature_celsius, 24)`,
		`clamp_max(temperature_celsius, 24)`,
		`sort(temperature_celsius)`,
		`sort_desc(temperature_celsius)`,

		// Helpers.
		`time()`,
		`timestamp(http_requests_total)`,
		`scalar(temperature_celsius{room="does-not-exist"})`,
		`scalar(sum(temperature_celsius))`,
		`vector(3.5)`,
		`vector(time())`,
		`-http_requests_total`,
		`+http_requests_total`,
		`- - 2`,

		// Aggregations.
		`sum(http_requests_total)`,
		`avg(http_requests_total)`,
		`min(http_requests_total)`,
		`max(http_requests_total)`,
		`count(http_requests_total)`,
		`stddev(http_requests_total)`,
		`stdvar(http_requests_total)`,
		`group(http_requests_total)`,
		`sum by (service_name) (http_requests_total)`,
		`avg by (service_name) (http_requests_total)`,
		`sum without (instance, method) (http_requests_total)`,
		`count by (method) (http_requests_total)`,
		`sum by (__name__) (http_requests_total)`,
		`sum by (service_name) (rate(http_requests_total[5m]))`,
		`topk(2, http_requests_total)`,
		`bottomk(2, http_requests_total)`,
		`topk by (service_name) (1, http_requests_total)`,
		`bottomk by (service_name) (1, http_requests_total)`,
		`topk(0, http_requests_total)`,
		`topk(100, http_requests_total)`,
		`count_values("value", http_requests_total)`,
		`count_values by (service_name) ("value", http_requests_total)`,

		// Binary operators.
		`http_requests_total + 1`,
		`1 + http_requests_total`,
		`http_requests_total - 1`,
		`1 - http_requests_total`,
		`http_requests_total * 2`,
		`2 * http_requests_total`,
		`http_requests_total / 2`,
		`2 / http_requests_total`,
		`http_requests_total % 3`,
		`3 % http_requests_total`,
		`http_requests_total ^ 2`,
		`2 ^ http_requests_total`,
		`http_requests_total atan2 100`,
		`100 atan2 http_requests_total`,
		`http_requests_total > 250`,
		`250 < http_requests_total`,
		`http_requests_total >= 250`,
		`http_requests_total < 250`,
		`http_requests_total <= 250`,
		`http_requests_total == 250`,
		`http_requests_total != 250`,
		`http_requests_total > bool 250`,
		`http_requests_total == bool 250`,
		`http_requests_total{service_name="checkout"} / on(instance) temperature_celsius{service_name="checkout"}`,
		`http_requests_total{service_name="checkout"} / ignoring(method) temperature_celsius{service_name="checkout"}`,
		`http_requests_total{service_name="checkout"} == on(instance) temperature_celsius{service_name="checkout"}`,
		`http_requests_total{service_name="checkout"} == bool on(instance) temperature_celsius{service_name="checkout"}`,
		`http_requests_total * on(service_name) group_left(version) request_info`,
		`http_requests_total * on(service_name) group_left(version, team) request_info`,
		`request_info * on(service_name) group_right(team) http_requests_total`,
		`http_requests_total * ignoring(version) group_left(version) request_info`,
		`http_requests_total > on(service_name) group_left(version) request_info`,
		`http_requests_total > bool on(service_name) group_left(version) request_info`,
		`request_info < on(service_name) group_right(team) http_requests_total`,

		// Set operators.
		`http_requests_total and temperature_celsius`,
		`http_requests_total and on(service_name, instance) temperature_celsius`,
		`http_requests_total and on(instance) temperature_celsius{service_name="checkout"}`,
		`http_requests_total or on(service_name, instance) temperature_celsius`,
		`http_requests_total or ignoring(method, room) temperature_celsius`,
		`http_requests_total unless on(service_name, instance) temperature_celsius`,
		`http_requests_total{service_name="checkout"} unless on(instance) temperature_celsius{service_name="checkout"}`,
		`http_requests_total{service_name="missing"} or temperature_celsius`,
		`temperature_celsius and http_requests_total`,
		`temperature_celsius unless http_requests_total`,
		`http_requests_total and on(__name__) http_requests_total`,
		`http_requests_total or on(__name__) http_requests_total`,
		`http_requests_total unless on(__name__) http_requests_total`,
		`http_requests_total and on(service_name) request_info`,
		`http_requests_total or on(service_name) request_info`,
		`http_requests_total unless on(service_name) request_info`,

		// Label functions.
		`label_replace(http_requests_total, "service", "$1", "service_name", "(.*)")`,
		`label_replace(http_requests_total, "__name__", "renamed_$1", "__name__", "(.*)")`,
		`label_replace(http_requests_total, "foo", "bar", "no_such_label", ".*")`,
		`label_replace(http_requests_total, "instance", "i$1", "instance", "([0-9]+)")`,
		`label_join(http_requests_total, "combo", "-", "service_name", "instance", "method")`,
		`label_join(http_requests_total, "__name__", "_", "__name__", "method")`,
		`label_join(http_requests_total, "combo", "", "service_name", "instance")`,
		`label_replace(label_replace(http_requests_total{service_name="checkout"}, "instance", "same", "instance", ".*"), "method", "same", "method", ".*")`,

		// Absent.
		`absent(http_requests_total{service_name="missing", instance="x"})`,
		`absent(http_requests_total)`,
		`absent(http_requests_total{service_name=~"check.*"})`,
		`absent(temperature_celsius{service_name="missing", room=~"a|b"})`,
		`absent(nonexistent{job="a",job="b",foo="bar"})`,
		`absent_over_time(http_requests_total{service_name="missing"}[5m])`,
		`absent_over_time(http_requests_total[5m])`,
		`absent_over_time(temperature_celsius{service_name="missing",room=~"a|b"}[5m])`,
	}
}

func rangeCorpus() []string {
	return []string{
		`http_requests_total`,
		`http_requests_total{service_name="checkout"}`,
		`temperature_celsius offset 1m`,
		`rate(http_requests_total[5m])`,
		`increase(http_requests_total[5m])`,
		`irate(http_requests_total[5m])`,
		`rate(http_requests_total{service_name="cart"}[5m])`,
		`avg_over_time(temperature_celsius[5m])`,
		`last_over_time(http_requests_total[5m])`,
		`histogram_quantile(0.9, rate(http_request_duration_seconds_bucket[5m]))`,
		`sum by (service_name) (rate(http_requests_total[5m]))`,
		`topk by (service_name) (1, http_requests_total)`,
		`bottomk(2, http_requests_total)`,
		`count_values("value", http_requests_total)`,
		`http_requests_total / on(instance) temperature_celsius{service_name="checkout"}`,
		`http_requests_total * on(service_name) group_left(version) request_info`,
		`http_requests_total and on(service_name, instance) temperature_celsius`,
		`http_requests_total or on(service_name, instance) temperature_celsius`,
		`http_requests_total unless on(service_name, instance) temperature_celsius`,
		`label_replace(http_requests_total, "service", "$1", "service_name", "(.*)")`,
		`label_join(http_requests_total, "combo", "-", "service_name", "instance")`,
		`absent(http_requests_total{service_name="missing"})`,
		`absent_over_time(http_requests_total{service_name="missing"}[5m])`,
		`vector(3)`,
	}
}

func TestDifferentialInstant(t *testing.T) {
	engines := NewEngines(corpusData())
	defer engines.Close()
	ctx := context.Background()
	for _, expr := range instantCorpus() {
		t.Run(expr, func(t *testing.T) {
			if err := engines.Instant(ctx, expr, evalTime); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDifferentialRange(t *testing.T) {
	engines := NewEngines(corpusData())
	defer engines.Close()
	ctx := context.Background()
	start := baseTime.Add(3 * time.Minute)
	for _, expr := range rangeCorpus() {
		t.Run(expr, func(t *testing.T) {
			if err := engines.Range(ctx, expr, start, evalTime, time.Minute); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDifferentialRangeTwoMinuteStep(t *testing.T) {
	engines := NewEngines(corpusData())
	defer engines.Close()
	ctx := context.Background()
	for _, expr := range []string{
		`http_requests_total`,
		`rate(http_requests_total[5m])`,
		`sum by (service_name) (http_requests_total)`,
		`topk(1, http_requests_total)`,
		`http_requests_total and on(service_name, instance) temperature_celsius`,
	} {
		t.Run(expr, func(t *testing.T) {
			if err := engines.Range(ctx, expr, baseTime.Add(time.Minute), evalTime, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReferenceStorageSanity(t *testing.T) {
	data := corpusData()
	if len(data) == 0 {
		t.Fatal("empty corpus")
	}
	engines := NewEngines(data)
	defer engines.Close()
	if err := engines.Instant(context.Background(), `sum(http_requests_total)`, evalTime); err != nil {
		t.Fatal(err)
	}
}

func init() {
	// Fail loudly if the corpus data drifts away from the fixed evaluation time.
	if !baseTime.Before(evalTime) {
		panic(fmt.Sprintf("invalid corpus times: %v >= %v", baseTime, evalTime))
	}
}
