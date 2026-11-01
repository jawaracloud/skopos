//go:build integration

package loki

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mrofisr/skopos/internal/chclient"
	"github.com/mrofisr/skopos/internal/plan"
)

func TestIntegrationLokiAPI(t *testing.T) {
	baseURL := os.Getenv("CLICKHOUSE_URL")
	if baseURL == "" {
		t.Fatal("CLICKHOUSE_URL is required for integration tests")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "skopos_loki_" + suffix
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
	exec("CREATE TABLE `" + database + "`.`otel_logs` (" +
		"Timestamp DateTime64(9), Body String, ServiceName LowCardinality(String), " +
		"SeverityText LowCardinality(String), SeverityNumber UInt8, TraceId String, SpanId String, " +
		"EventName String, LogAttributes Map(String, String), ResourceAttributes Map(String, String)" +
		") ENGINE = MergeTree ORDER BY (ServiceName, Timestamp)")

	rows := []string{
		`{"Timestamp":"2026-11-01 00:00:00.000000000","Body":"request timeout after 30s","ServiceName":"checkout","SeverityText":"ERROR","SeverityNumber":17,"TraceId":"4bf92f3577b34da6a3ce929d0e0e4736","SpanId":"00f067aa0ba902b7","EventName":"","LogAttributes":{"request_id":"abc"},"ResourceAttributes":{"k8s.pod.name":"checkout-abc"}}`,
		`{"Timestamp":"2026-11-01 00:00:01.000000000","Body":"request ok","ServiceName":"checkout","SeverityText":"INFO","SeverityNumber":9,"TraceId":"4bf92f3577b34da6a3ce929d0e0e4736","SpanId":"00f067aa0ba902b8","EventName":"","LogAttributes":{"request_id":"def"},"ResourceAttributes":{"k8s.pod.name":"checkout-abc"}}`,
	}
	exec("INSERT INTO `" + database + "`.`otel_logs` FORMAT JSONEachRow\n" + strings.Join(rows, "\n"))

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
	api := NewAPI(fetcher, "test-version")
	api.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	api.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	query := url.QueryEscape(`{service_name="checkout"} |= "timeout"`)
	status, body := get(`/loki/api/v1/query_range?query=` + query + `&start=1793491200000000000&end=1793494800000000000&limit=10`)
	if status != http.StatusOK {
		t.Fatalf("query_range = %d %s", status, body)
	}
	var response map[string]any
	_ = json.Unmarshal(body, &response)
	data := response["data"].(map[string]any)
	result := data["result"].([]any)
	if len(result) != 1 {
		t.Fatalf("query result = %#v", result)
	}
	values := result[0].(map[string]any)["values"].([]any)
	if len(values) != 1 || values[0].([]any)[1] != "request timeout after 30s" {
		t.Fatalf("values = %#v", values)
	}

	status, body = get(`/loki/api/v1/labels?start=1793491200000000000&end=1793494800000000000`)
	if status != http.StatusOK {
		t.Fatalf("labels = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &response)
	labels := response["data"].([]any)
	found := false
	for _, label := range labels {
		if label == "k8s_pod_name" {
			found = true
		}
	}
	if !found {
		t.Fatalf("labels missing normalized attribute: %#v", labels)
	}

	status, body = get(`/loki/api/v1/label/service_name/values`)
	if status != http.StatusOK {
		t.Fatalf("values = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &response)
	values = response["data"].([]any)
	if len(values) != 1 || values[0] != "checkout" {
		t.Fatalf("label values = %#v", values)
	}

	status, body = get(`/loki/api/v1/series?match[]=` + url.QueryEscape(`{service_name="checkout"}`))
	if status != http.StatusOK {
		t.Fatalf("series = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &response)
	series := response["data"].([]any)
	if len(series) != 2 {
		t.Fatalf("series = %#v", series)
	}
}
