//go:build integration

package sqlgen

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/obi-wrapper/skopos/internal/chclient"
	"github.com/obi-wrapper/skopos/internal/plan"
)

// TestIntegrationEmittedSQL executes emitter output against a real ClickHouse
// server. It covers the placeholder syntax, identifier quoting, DateTime64
// parameters, map-key parameters, and the limit parameter end to end.
func TestIntegrationEmittedSQL(t *testing.T) {
	baseURL := os.Getenv("CLICKHOUSE_URL")
	if baseURL == "" {
		t.Fatal("CLICKHOUSE_URL is required for integration tests")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "skopos_sqlgen_" + suffix
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
	t.Cleanup(func() {
		exec("DROP DATABASE IF EXISTS `" + database + "` SYNC")
	})
	exec("CREATE TABLE `" + database + "`.`otel_metrics_gauge` (" +
		"MetricName LowCardinality(String), TimeUnix DateTime, Value Float64, " +
		"ServiceName String, Attributes Map(String, String)" +
		") ENGINE = MergeTree ORDER BY (MetricName, TimeUnix)")
	exec("INSERT INTO `" + database + "`.`otel_metrics_gauge` FORMAT JSONEachRow\n" +
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:00:30","Value":1.5,"ServiceName":"checkout","Attributes":{"service.name":"checkout"}}` + "\n" +
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:01:30","Value":2.5,"ServiceName":"checkout","Attributes":{"service.name":"checkout"}}` + "\n" +
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:02:30","Value":9.0,"ServiceName":"cart","Attributes":{"service.name":"cart"}}` + "\n" +
		`{"MetricName":"other_total","TimeUnix":"2026-11-01 00:03:30","Value":7.0,"ServiceName":"checkout","Attributes":{"service.name":"checkout"}}`)

	schema := plan.DefaultSchema(database, plan.DefaultTableNames())
	emitter, err := New(schema, Limits{MaxLimit: 1000, MaxRange: 24 * time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	query := plan.Query{
		Signal: plan.SignalMetrics,
		Table:  "otel_metrics_gauge",
		Select: []plan.SelectItem{
			{Expr: plan.Col("MetricName")},
			{Expr: plan.Col("Value")},
			{Expr: plan.Col("TimeUnix")},
		},
		Where: plan.Bin("AND",
			plan.Bin("=", plan.Col("MetricName"), plan.String("metric", "http_requests_total")),
			plan.Bin("=",
				plan.Map(plan.Col("Attributes"), plan.String("label", "service.name")),
				plan.String("service", "checkout"),
			),
		),
		TimeColumn: "TimeUnix",
		TimeRange: &plan.TimeRange{
			Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 11, 1, 0, 2, 0, 0, time.UTC),
		},
		OrderBy: []plan.Order{{By: "TimeUnix"}},
		Limit:   10,
	}
	emitted, err := emitter.Emit(query)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		BaseURL:          baseURL,
		Database:         database,
		Timeout:          10 * time.Second,
		MaxExecutionTime: 10 * time.Second,
		MaxMemoryUsage:   256 << 20,
		MaxRowsToRead:    1_000_000,
		MaxResultRows:    1000,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	got, err := client.Query(context.Background(), emitted.SQL, emitted.Params)
	if err != nil {
		t.Fatalf("emitted SQL failed: %v\nSQL:\n%s", err, emitted.SQL)
	}
	if got.Len() != 2 {
		t.Fatalf("rows = %d, want 2: %#v", got.Len(), got.Rows)
	}
	want := []float64{1.5, 2.5}
	for i, value := range want {
		gotValue, ok := got.Float64(i, "Value")
		if !ok || gotValue != value {
			t.Fatalf("row %d Value = %v ok=%v, want %v", i, gotValue, ok, value)
		}
	}
}
