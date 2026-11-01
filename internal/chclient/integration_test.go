//go:build integration

package chclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/obi-wrapper/skopos/internal/plan"
)

func TestIntegrationQueryParameters(t *testing.T) {
	baseURL := os.Getenv("CLICKHOUSE_URL")
	if baseURL == "" {
		t.Fatal("CLICKHOUSE_URL is required for integration tests")
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "skopos_it_" + suffix
	user := "skopos_it_user_" + suffix
	password := "pw" + suffix
	table := "it_metrics"

	admin := &http.Client{Timeout: 15 * time.Second}
	adminExec := func(query string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL, strings.NewReader(query))
		if err != nil {
			t.Fatalf("build admin request: %v", err)
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("admin request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin query failed: status %d: %s\nquery: %s", resp.StatusCode, body, query)
		}
	}

	adminExec("CREATE DATABASE `" + database + "`")
	t.Cleanup(func() {
		adminExec("DROP DATABASE IF EXISTS `" + database + "` SYNC")
		adminExec("DROP USER IF EXISTS `" + user + "`")
	})

	adminExec("CREATE TABLE `" + database + "`.`" + table + "` (" +
		"MetricName LowCardinality(String), " +
		"TimeUnix DateTime64(9), " +
		"Value Float64, " +
		"ServiceName String, " +
		"Attributes Map(String, String)" +
		") ENGINE = MergeTree ORDER BY (MetricName, TimeUnix)")

	adminExec("CREATE USER `" + user + "` IDENTIFIED WITH sha256_password BY '" + password + "' SETTINGS readonly = 2")
	adminExec("GRANT SELECT ON `" + database + "`.* TO `" + user + "`")

	rows := strings.Join([]string{
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:00:30.000000000","Value":1.5,"ServiceName":"checkout","Attributes":{"service.name":"checkout"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:01:30.000000000","Value":2.5,"ServiceName":"checkout","Attributes":{"service.name":"checkout"}}`,
		`{"MetricName":"http_requests_total","TimeUnix":"2026-11-01 00:02:30.000000000","Value":9.0,"ServiceName":"cart","Attributes":{"service.name":"cart"}}`,
		`{"MetricName":"other_total","TimeUnix":"2026-11-01 00:03:30.000000000","Value":7.0,"ServiceName":"checkout","Attributes":{"service.name":"checkout"}}`,
	}, "\n")
	adminExec("INSERT INTO `" + database + "`.`" + table + "` FORMAT JSONEachRow\n" + rows)

	client, err := New(Config{
		BaseURL:          baseURL,
		Database:         database,
		User:             user,
		Password:         password,
		Timeout:          10 * time.Second,
		MaxExecutionTime: 10 * time.Second,
		MaxMemoryUsage:   256 << 20,
		MaxRowsToRead:    1_000_000,
		MaxResultRows:    1000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 1, 0, 3, 0, 0, time.UTC)
	got, err := client.Query(context.Background(),
		"SELECT MetricName, Value, ServiceName, TimeUnix, Attributes FROM `"+database+"`.`"+table+"` "+
			"WHERE MetricName = {metric:String} "+
			"AND TimeUnix >= {start:DateTime64(9)} "+
			"AND TimeUnix < {end:DateTime64(9)} "+
			"AND Attributes[{key:String}] = {value:String} "+
			"AND has({services:Array(String)}, ServiceName) "+
			"ORDER BY TimeUnix LIMIT {limit:UInt64}",
		[]plan.Param{
			plan.String("metric", "http_requests_total"),
			plan.Time("start", start),
			plan.Time("end", end),
			plan.String("key", "service.name"),
			plan.String("value", "checkout"),
			plan.Strings("services", []string{"checkout", "cart"}),
			plan.Uint("limit", 10),
		})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("rows = %d, want 2: %#v", got.Len(), got.Rows)
	}
	wantValues := []float64{1.5, 2.5}
	for i, want := range wantValues {
		value, ok := got.Float64(i, "Value")
		if !ok || value != want {
			t.Fatalf("row %d Value = %v ok=%v, want %v", i, value, ok, want)
		}
		service, ok := got.String(i, "ServiceName")
		if !ok || service != "checkout" {
			t.Fatalf("row %d ServiceName = %q ok=%v", i, service, ok)
		}
	}

	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// Exercise the Array(String) encoder with quotes, backslashes, and control
	// characters. ClickHouse parses the HTTP parameter using its text format.
	escapedValues := []string{`a'b\c`, "line\nbreak\ttab", "ctrl\x01value"}
	escaped, err := client.Query(context.Background(),
		"SELECT arrayJoin({values:Array(String)}) AS v",
		[]plan.Param{plan.Strings("values", escapedValues)})
	if err != nil {
		t.Fatalf("array escape query: %v", err)
	}
	if escaped.Len() != len(escapedValues) {
		t.Fatalf("array escape rows = %d, want %d: %#v", escaped.Len(), len(escapedValues), escaped.Rows)
	}
	for i, want := range escapedValues {
		got, ok := escaped.String(i, "v")
		if !ok || got != want {
			t.Fatalf("array escape row %d = %q ok=%v, want %q", i, got, ok, want)
		}
	}

	if _, err := client.Query(context.Background(), "INSERT INTO `"+database+"`.`"+table+"` VALUES ('x', now(), 1, 'x', map())", nil); err == nil {
		t.Fatal("write statement was accepted")
	}
}
