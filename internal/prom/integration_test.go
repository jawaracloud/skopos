//go:build integration

package prom

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrofisr/skopos/internal/chclient"
	"github.com/mrofisr/skopos/internal/plan"
)

func TestIntegrationPrometheusAPI(t *testing.T) {
	baseURL := os.Getenv("CLICKHOUSE_URL")
	if baseURL == "" {
		t.Fatal("CLICKHOUSE_URL is required for integration tests")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "skopos_prom_" + suffix
	admin := &http.Client{Timeout: 15 * time.Second}
	exec := func(query string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL, strings.NewReader(query))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query failed: status %d: %s\nquery: %s", resp.StatusCode, body, query)
		}
	}
	exec("CREATE DATABASE `" + database + "`")
	t.Cleanup(func() { exec("DROP DATABASE IF EXISTS `" + database + "` SYNC") })
	exec("CREATE TABLE `" + database + "`.`otel_metrics_gauge` (" +
		"MetricName LowCardinality(String), TimeUnix DateTime, Value Float64, " +
		"ServiceName LowCardinality(String), MetricDescription String, MetricUnit String, " +
		"Attributes Map(String, String)) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix)")
	exec("CREATE TABLE `" + database + "`.`otel_metrics_sum` (" +
		"MetricName LowCardinality(String), TimeUnix DateTime, Value Float64, " +
		"ServiceName LowCardinality(String), MetricDescription String, MetricUnit String, " +
		"Attributes Map(String, String)) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix)")
	exec("CREATE TABLE `" + database + "`.`otel_metrics_histogram` (" +
		"MetricName LowCardinality(String), TimeUnix DateTime, " +
		"ServiceName LowCardinality(String), MetricDescription String, MetricUnit String, " +
		"Attributes Map(String, String), Count UInt64, Sum Float64, " +
		"BucketCounts Array(UInt64), ExplicitBounds Array(Float64)) " +
		"ENGINE = MergeTree ORDER BY (MetricName, TimeUnix)")

	gaugeRows := []string{
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:00:30","Value":1,"ServiceName":"checkout","MetricDescription":"requests","MetricUnit":"1","Attributes":{"http.method":"GET"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:01:30","Value":2,"ServiceName":"checkout","MetricDescription":"requests","MetricUnit":"1","Attributes":{"http.method":"GET"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:02:30","Value":3,"ServiceName":"checkout","MetricDescription":"requests","MetricUnit":"1","Attributes":{"http.method":"GET"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:03:30","Value":4,"ServiceName":"checkout","MetricDescription":"requests","MetricUnit":"1","Attributes":{"http.method":"GET"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:04:30","Value":5,"ServiceName":"checkout","MetricDescription":"requests","MetricUnit":"1","Attributes":{"http.method":"GET"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:05:30","Value":6,"ServiceName":"checkout","MetricDescription":"requests","MetricUnit":"1","Attributes":{"http.method":"GET"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:05:30","Value":60,"ServiceName":"cart","MetricDescription":"requests","MetricUnit":"1","Attributes":{"http.method":"GET"}}`,
	}
	exec("INSERT INTO `" + database + "`.`otel_metrics_gauge` FORMAT JSONEachRow\n" + strings.Join(gaugeRows, "\n"))
	histRows := []string{
		`{"MetricName":"http_request_duration","TimeUnix":"2026-11-01 00:00:00","ServiceName":"checkout","MetricDescription":"duration","MetricUnit":"s","Attributes":{"http.method":"GET"},"Count":5,"Sum":2.5,"BucketCounts":[1,2,1,1],"ExplicitBounds":[0.1,0.5,1]}`,
		`{"MetricName":"http_request_duration","TimeUnix":"2026-11-01 00:05:00","ServiceName":"checkout","MetricDescription":"duration","MetricUnit":"s","Attributes":{"http.method":"GET"},"Count":10,"Sum":5.0,"BucketCounts":[2,4,2,2],"ExplicitBounds":[0.1,0.5,1]}`,
	}
	exec("INSERT INTO `" + database + "`.`otel_metrics_histogram` FORMAT JSONEachRow\n" + strings.Join(histRows, "\n"))

	client, err := chclient.New(chclient.Config{
		BaseURL:          baseURL,
		Database:         database,
		Timeout:          10 * time.Second,
		MaxExecutionTime: 10 * time.Second,
		MaxMemoryUsage:   256 << 20,
		MaxRowsToRead:    1_000_000,
		MaxResultRows:    100_000,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	names := plan.DefaultTableNames()
	schema := plan.DefaultSchema(database, names)
	fetcher, err := NewClickHouseFetcher(client, schema, names, 100_000)
	if err != nil {
		t.Fatalf("NewClickHouseFetcher: %v", err)
	}
	engine := NewEngine(fetcher)
	api := NewAPI(engine, fetcher, "test-version")
	api.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 6, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	api.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	get := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK || body["status"] != "success" {
			t.Fatalf("GET %s = %d %#v", path, resp.StatusCode, body)
		}
		return body["data"].(map[string]any)
	}

	data := get("/api/v1/query?query=" + url.QueryEscape(`http_requests_total{service_name="checkout"}`) + "&time=2026-11-01T00:06:00Z")
	result := data["result"].([]any)
	if len(result) != 1 {
		t.Fatalf("instant result = %#v", result)
	}
	value := result[0].(map[string]any)["value"].([]any)
	if value[1] != "6" {
		t.Fatalf("instant value = %#v, want 6", value)
	}
	metric := result[0].(map[string]any)["metric"].(map[string]any)
	if metric["http_method"] != "GET" {
		t.Fatalf("normalized attribute label missing: %#v", metric)
	}

	data = get("/api/v1/query_range?query=" + url.QueryEscape(`http_requests_total{service_name="checkout"}`) + "&start=2026-11-01T00:03:00Z&end=2026-11-01T00:06:00Z&step=60")
	matrix := data["result"].([]any)
	if len(matrix) != 1 {
		t.Fatalf("range result = %#v", matrix)
	}
	values := matrix[0].(map[string]any)["values"].([]any)
	if len(values) != 4 {
		t.Fatalf("range values = %#v", values)
	}
	for i, want := range []string{"3", "4", "5", "6"} {
		if got := values[i].([]any)[1]; got != want {
			t.Fatalf("range value %d = %v, want %s", i, got, want)
		}
	}

	data = get("/api/v1/query?query=" + url.QueryEscape(`histogram_quantile(0.5, rate(http_request_duration_bucket[5m]))`) + "&time=2026-11-01T00:05:00Z")
	histResult := data["result"].([]any)
	if len(histResult) != 1 {
		t.Fatalf("histogram result = %#v", histResult)
	}
	histValue := histResult[0].(map[string]any)["value"].([]any)
	if got, err := strconvParse(histValue[1]); err != nil || math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("histogram value = %#v, want 0.4", histValue)
	}

	data = get("/api/v1/metadata?metric=http_requests_total")
	if _, ok := data["http_requests_total"]; !ok {
		t.Fatalf("metadata missing metric: %#v", data)
	}

}

func strconvParse(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("not a string")
	}
	return strconv.ParseFloat(s, 64)
}
