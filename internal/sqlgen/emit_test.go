package sqlgen

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obi-wrapper/skopos/internal/plan"
)

var update = flag.Bool("update", false, "update golden files")

type goldenParam struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

func testSchema() *plan.Schema {
	return plan.DefaultSchema("otel", plan.DefaultTableNames())
}

func testEmitter(t *testing.T) *Emitter {
	t.Helper()
	e, err := New(testSchema(), Limits{MaxLimit: 10000, MaxRange: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func paramsForGolden(params []plan.Param) []goldenParam {
	out := make([]goldenParam, 0, len(params))
	for _, p := range params {
		var v any = p.Value
		switch x := p.Value.(type) {
		case time.Time:
			v = x.UTC().Format(time.RFC3339Nano)
		case []string:
			v = x
		}
		out = append(out, goldenParam{Name: p.Name, Type: p.Type, Value: v})
	}
	return out
}

func checkGolden(t *testing.T, name string, got Result) {
	t.Helper()
	sqlPath := filepath.Join("testdata", name+".sql")
	jsonPath := filepath.Join("testdata", name+".json")

	if *update {
		if err := os.WriteFile(sqlPath, []byte(got.SQL), 0o644); err != nil {
			t.Fatalf("write golden SQL: %v", err)
		}
		data, err := json.MarshalIndent(paramsForGolden(got.Params), "", "  ")
		if err != nil {
			t.Fatalf("marshal golden params: %v", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
			t.Fatalf("write golden params: %v", err)
		}
		return
	}

	wantSQL, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read golden SQL: %v", err)
	}
	if got.SQL != string(wantSQL) {
		t.Fatalf("SQL mismatch\n--- got ---\n%s\n--- want ---\n%s", got.SQL, wantSQL)
	}

	wantJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read golden params: %v", err)
	}
	gotJSON, err := json.MarshalIndent(paramsForGolden(got.Params), "", "  ")
	if err != nil {
		t.Fatalf("marshal got params: %v", err)
	}
	gotJSON = append(gotJSON, '\n')
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("params mismatch\n--- got ---\n%s\n--- want ---\n%s", gotJSON, wantJSON)
	}
}

func TestEmitMetricsSimple(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)

	q := plan.Query{
		Signal: plan.SignalMetrics,
		Table:  "otel_metrics_gauge",
		Select: []plan.SelectItem{
			{Expr: plan.Col("MetricName")},
			{Expr: plan.Col("TimeUnix")},
			{Expr: plan.Col("Value")},
		},
		TimeColumn: "TimeUnix",
		TimeRange:  &plan.TimeRange{Start: start, End: end},
		OrderBy:    []plan.Order{{By: "TimeUnix"}},
		Limit:      100,
	}

	got, err := testEmitter(t).Emit(q)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	checkGolden(t, "metrics_simple", got)
}
