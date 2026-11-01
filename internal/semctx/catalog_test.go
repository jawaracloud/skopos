package semctx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validCatalogJSON = `{
  "schema": "ctx/v1",
  "version": "2026-11-01.1",
  "metrics": {
    "demo_temperature_celsius": {
      "description": "Demo temperature",
      "type": "gauge",
      "unit": "Cel",
      "role": "resource",
      "direction": "neutral",
      "owner": "platform",
      "service": "checkout",
      "query": "demo_temperature_celsius",
      "runbook": "https://runbooks.example.com/temperature",
      "labels": {"sensor": "sensor id"},
      "related": ["demo_requests_total"],
      "caveats": ["Demo fixture"],
      "flags": ["slo_metric"]
    },
    "demo_requests_total": {
      "description": "Demo requests",
      "type": "counter",
      "unit": "requests",
      "role": "traffic",
      "direction": "higher_is_worse",
      "service": "checkout"
    }
  },
  "services": {
    "checkout": {
      "description": "Checkout API",
      "owner": "payments",
      "runbook": "https://runbooks.example.com/checkout",
      "metrics": ["demo_temperature_celsius", "demo_requests_total"],
      "logs": ["checkout error logs"],
      "traces": ["GET /checkout"]
    }
  },
  "logs": {
    "checkout.error": {
      "description": "Checkout errors",
      "service": "checkout",
      "event": "exception",
      "severity": ["ERROR", "FATAL"],
      "query": "{service_name=\"checkout\"} | json | level=\"error\"",
      "runbook": "https://runbooks.example.com/checkout/errors",
      "fields": {"level": "log level"},
      "caveats": ["Sampled errors"],
      "flags": ["slo_metric"]
    }
  },
  "traces": {
    "checkout.get": {
      "description": "Checkout request path",
      "service": "checkout",
      "operation": "GET /checkout",
      "kind": "server",
      "critical_path": true,
      "downstream": ["payments"],
      "query": "{ resource.service.name = \"checkout\" }",
      "runbook": "https://runbooks.example.com/checkout",
      "caveats": ["Sampled traces"],
      "flags": ["slo_metric"]
    }
  }
}`

func mustCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := Parse(strings.NewReader(validCatalogJSON))
	if err != nil {
		t.Fatalf("Parse valid catalog: %v", err)
	}
	return catalog
}

func TestParseValid(t *testing.T) {
	catalog := mustCatalog(t)
	if catalog.Schema != SchemaVersion || catalog.Version != "2026-11-01.1" {
		t.Fatalf("catalog header = %#v", catalog)
	}
	if len(catalog.Metrics) != 2 || len(catalog.Services) != 1 || len(catalog.Logs) != 1 || len(catalog.Traces) != 1 {
		t.Fatalf("catalog size = %d/%d/%d/%d", len(catalog.Metrics), len(catalog.Services), len(catalog.Logs), len(catalog.Traces))
	}
	if catalog.Metrics["demo_temperature_celsius"].Owner != "platform" {
		t.Fatalf("metric = %#v", catalog.Metrics["demo_temperature_celsius"])
	}
}

func TestEmptyCatalog(t *testing.T) {
	catalog, err := LoadFile("")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Schema != SchemaVersion || catalog.Version != "empty" || len(catalog.Metrics) != 0 {
		t.Fatalf("empty catalog = %#v", catalog)
	}
}

func TestExampleCatalogLoads(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "examples", "context", "catalog.json")
	catalog, err := LoadFile(path)
	if err != nil {
		t.Fatalf("example catalog: %v", err)
	}
	if catalog.Version == "" || len(catalog.Metrics) == 0 {
		t.Fatalf("example catalog = %#v", catalog)
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(validCatalogJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadFile(path)
	if err != nil || len(catalog.Metrics) != 2 {
		t.Fatalf("LoadFile = %#v, %v", catalog, err)
	}
}

func TestParseRejectsInvalidCatalog(t *testing.T) {
	var labelJSON strings.Builder
	for i := 0; i <= maxLabels; i++ {
		fmt.Fprintf(&labelJSON, `"label%d":"v",`, i)
	}
	tooManyLabels := `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","labels":{` + labelJSON.String() + `"z":"v"}}}}`
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"missing schema", `{"version":"v1","metrics":{}}`, "schema"},
		{"wrong schema", `{"schema":"ctx/v2","version":"v1"}`, "schema"},
		{"missing version", `{"schema":"ctx/v1","version":""}`, "version"},
		{"unknown field", `{"schema":"ctx/v1","version":"v1","unknown":true}`, "unknown field"},
		{"trailing JSON", `{"schema":"ctx/v1","version":"v1"} {}`, "more than one JSON value"},
		{"invalid metric name", `{"schema":"ctx/v1","version":"v1","metrics":{"bad-name":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral"}}}`, "metric name"},
		{"empty description", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"","type":"gauge","unit":"1","role":"resource","direction":"neutral"}}}`, "description"},
		{"long description", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"` + strings.Repeat("x", maxDescription+1) + `","type":"gauge","unit":"1","role":"resource","direction":"neutral"}}}`, "description"},
		{"invalid type", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"unknown","unit":"1","role":"resource","direction":"neutral"}}}`, "type"},
		{"invalid unit", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"` + strings.Repeat("u", maxUnit+1) + `","role":"resource","direction":"neutral"}}}`, "unit"},
		{"invalid role", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"guess","direction":"neutral"}}}`, "role"},
		{"invalid direction", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"up"}}}`, "direction"},
		{"invalid owner", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","owner":" owner "}}}`, "owner"},
		{"invalid runbook", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","runbook":"ftp://example.com"}}}`, "runbook"},
		{"query newline", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","query":"up\n"}}}`, "query"},
		{"too many labels", tooManyLabels, "labels"},
		{"too many caveats", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","caveats":["a","b","c"]}}}`, "caveats"},
		{"too many flags", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","flags":["a","b","c","d","e","f","g","h","i"]}}}`, "flags"},
		{"invalid flag", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","flags":["Bad"]}}}`, "flag"},
		{"duplicate flag", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","flags":["ok","ok"]}}}`, "duplicate flag"},
		{"missing related", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","related":["missing"]}}}`, "related metric"},
		{"missing service ref", `{"schema":"ctx/v1","version":"v1","metrics":{"m":{"description":"x","type":"gauge","unit":"1","role":"resource","direction":"neutral","service":"missing"}}}`, "service"},
		{"service missing description", `{"schema":"ctx/v1","version":"v1","services":{"checkout":{"metrics":[]}}}`, "description"},
		{"service missing metric", `{"schema":"ctx/v1","version":"v1","services":{"checkout":{"description":"x","metrics":["missing"]}}}`, "not in the catalog"},
		{"invalid log id", `{"schema":"ctx/v1","version":"v1","services":{"checkout":{"description":"x"}},"logs":{"bad id":{"description":"x","service":"checkout"}}}`, "log context id"},
		{"log missing service", `{"schema":"ctx/v1","version":"v1","logs":{"checkout.error":{"description":"x","service":"missing"}}}`, "service"},
		{"invalid severity", `{"schema":"ctx/v1","version":"v1","services":{"checkout":{"description":"x"}},"logs":{"checkout.error":{"description":"x","service":"checkout","severity":["LOUD"]}}}`, "severity"},
		{"invalid trace kind", `{"schema":"ctx/v1","version":"v1","services":{"checkout":{"description":"x"}},"traces":{"checkout.get":{"description":"x","service":"checkout","operation":"GET /checkout","kind":"weird"}}}`, "kind"},
		{"trace missing operation", `{"schema":"ctx/v1","version":"v1","services":{"checkout":{"description":"x"}},"traces":{"checkout.get":{"description":"x","service":"checkout"}}}`, "operation"},
		{"trace missing service", `{"schema":"ctx/v1","version":"v1","traces":{"checkout.get":{"description":"x","service":"missing","operation":"GET /checkout"}}}`, "service"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.raw))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseRejectsDuplicateKeys(t *testing.T) {
	raw := `{"schema":"ctx/v1","version":"v1","metrics":{},"metrics":{}}`
	_, err := Parse(strings.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseRejectsOversizedCatalog(t *testing.T) {
	raw := `{"schema":"ctx/v1","version":"v1","description":"` + strings.Repeat("x", maxCatalogBytes) + `"}`
	_, err := Parse(strings.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}
