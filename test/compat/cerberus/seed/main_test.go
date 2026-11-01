package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderChartDefaults(t *testing.T) {
	raw := `CREATE TABLE {{ .Values.clickhouse.database }}.x (
    Timestamp DateTime64(9)
) ENGINE = MergeTree()
ORDER BY Timestamp
{{- if .Values.clickhouse.storage.enabled }}
TTL Timestamp + toIntervalDay({{ .Values.clickhouse.storage.hotDays }}) TO VOLUME '{{ .Values.clickhouse.storage.coldVolume }}',
    Timestamp + toIntervalDay({{ .Values.clickhouse.storage.totalDays }}) DELETE
{{- else }}
TTL Timestamp + toIntervalDay({{ .Values.clickhouse.ttl.tracesDays }})
{{- end }}
SETTINGS index_granularity=8192{{- if .Values.clickhouse.storage.enabled }}, storage_policy = 'tiered'{{- end }};`

	got := renderChartDefaults(raw, "otel")
	if strings.Contains(got, "{{") {
		t.Fatalf("rendered schema still contains a placeholder:\n%s", got)
	}
	if strings.Contains(got, "storage_policy") || strings.Contains(got, "TO VOLUME") {
		t.Fatalf("rendered schema contains storage-enabled DDL:\n%s", got)
	}
	if !strings.Contains(got, "toIntervalDay(20000)") {
		t.Fatalf("rendered schema did not keep the local TTL branch:\n%s", got)
	}
	if !strings.Contains(got, "CREATE TABLE otel.x") {
		t.Fatalf("rendered schema did not substitute the database:\n%s", got)
	}
}

func TestChartSchemaRendersWithLocalDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "deploy", "helm", "skopos", "files", "schema.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	got := renderChartDefaults(string(raw), "otel")
	if strings.Contains(got, "{{") {
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if strings.Contains(line, "{{") {
				t.Errorf("unrendered placeholder: %s", line)
			}
		}
		t.Fatal("chart schema did not render fully")
	}
	if strings.Contains(got, "storage_policy = 'tiered'") {
		t.Fatal("chart schema rendered the storage policy with storage disabled")
	}
	if !strings.Contains(got, "idx_body Body TYPE text") {
		t.Fatal("chart schema is missing the case-sensitive body index")
	}
	if strings.Count(got, "CREATE TABLE IF NOT EXISTS") != 8 {
		t.Fatalf("expected 8 tables in the rendered schema, got %d", strings.Count(got, "CREATE TABLE IF NOT EXISTS"))
	}
}
