package semctx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func apiFor(t *testing.T, catalog *Catalog) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	NewAPI(catalog).Register(mux)
	return mux
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestMetricMinimal(t *testing.T) {
	catalog := mustCatalog(t)
	rec := get(t, apiFor(t, catalog), "/api/v1/context/metrics/demo_temperature_celsius")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	body := decodeMap(t, rec)
	if body["known"] != true || body["role"] != "resource" || body["unit"] != "Cel" {
		t.Fatalf("body = %v", body)
	}
	for _, key := range []string{"query", "labels", "related", "caveats", "provenance"} {
		if _, ok := body[key]; ok {
			t.Fatalf("minimal response contains %q: %s", key, rec.Body)
		}
	}
	if rec.Body.Len() > 900 {
		t.Fatalf("minimal response is too large: %d bytes", rec.Body.Len())
	}
	if got := rec.Header().Get("X-Skopos-Context-Version"); got != catalog.Version {
		t.Fatalf("context version header = %q", got)
	}
}

func TestMetricStandardAndFull(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	standard := decodeMap(t, get(t, handler, "/api/v1/context/metrics/demo_temperature_celsius?detail=standard"))
	for _, key := range []string{"query", "runbook", "labels"} {
		if _, ok := standard[key]; !ok {
			t.Fatalf("standard response missing %q: %v", key, standard)
		}
	}
	for _, key := range []string{"related", "caveats", "provenance"} {
		if _, ok := standard[key]; ok {
			t.Fatalf("standard response unexpectedly contains %q", key)
		}
	}
	full := decodeMap(t, get(t, handler, "/api/v1/context/metrics/demo_temperature_celsius?detail=full"))
	for _, key := range []string{"query", "runbook", "labels", "related", "caveats", "provenance"} {
		if _, ok := full[key]; !ok {
			t.Fatalf("full response missing %q: %v", key, full)
		}
	}
}

func TestMetricInclude(t *testing.T) {
	body := decodeMap(t, get(t, apiFor(t, mustCatalog(t)),
		"/api/v1/context/metrics/demo_temperature_celsius?detail=minimal&include=labels,query"))
	if _, ok := body["labels"]; !ok {
		t.Fatal("include=labels did not add labels")
	}
	if _, ok := body["query"]; !ok {
		t.Fatal("include=query did not add query")
	}
	if _, ok := body["runbook"]; ok {
		t.Fatal("unrequested runbook was included")
	}
}

func TestUnknownMetricIsExplicitAndSmall(t *testing.T) {
	rec := get(t, apiFor(t, EmptyCatalog()), "/api/v1/context/metrics/missing_metric_total")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeMap(t, rec)
	if body["known"] != false {
		t.Fatalf("known = %v", body["known"])
	}
	flags, _ := body["flags"].([]any)
	if len(flags) != 1 || flags[0] != "missing_context" {
		t.Fatalf("flags = %v", flags)
	}
	for _, key := range []string{"type", "unit", "role", "description", "query", "labels"} {
		if _, ok := body[key]; ok {
			t.Fatalf("unknown metric leaked %q: %s", key, rec.Body)
		}
	}
	if rec.Body.Len() > 200 {
		t.Fatalf("unknown response is too large: %d bytes", rec.Body.Len())
	}
}

func TestInvalidQueryParameters(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	for _, path := range []string{
		"/api/v1/context/metrics/demo_temperature_celsius?detail=raw",
		"/api/v1/context/metrics/demo_temperature_celsius?include=secrets",
		"/api/v1/context/metrics?limit=0",
		"/api/v1/context/metrics?limit=51",
		"/api/v1/context/metrics?role=guess",
		"/api/v1/context/services/checkout?limit=21",
	} {
		rec := get(t, handler, path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body)
		}
	}
}

func TestListIsCompactAndBounded(t *testing.T) {
	metrics := make(map[string]Metric, 30)
	for i := 0; i < 30; i++ {
		metrics[fmt.Sprintf("metric_%02d", i)] = Metric{
			Description: "Demo metric", Type: "gauge", Unit: "1",
			Role: "resource", Direction: "neutral", Service: "checkout",
		}
	}
	catalog := &Catalog{Schema: SchemaVersion, Version: "v1", Metrics: metrics}
	rec := get(t, apiFor(t, catalog), "/api/v1/context/metrics?service=checkout&limit=20")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeMap(t, rec)
	items, _ := body["items"].([]any)
	if len(items) != 20 || body["total"] != float64(30) || body["has_more"] != true {
		t.Fatalf("list = %v", body)
	}
	first := items[0].(map[string]any)
	for _, key := range []string{"description", "query", "labels", "related", "caveats"} {
		if _, ok := first[key]; ok {
			t.Fatalf("list leaked %q: %v", key, first)
		}
	}
	if rec.Body.Len() > 5000 {
		t.Fatalf("list response is too large: %d bytes", rec.Body.Len())
	}
}

func TestServiceContext(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	rec := get(t, handler, "/api/v1/context/services/checkout")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeMap(t, rec)
	if body["known"] != true || body["owner"] != "payments" {
		t.Fatalf("service = %v", body)
	}
	metrics, _ := body["metrics"].([]any)
	if len(metrics) != 2 {
		t.Fatalf("metrics = %v", metrics)
	}
	logs, _ := body["logs"].([]any)
	traces, _ := body["traces"].([]any)
	if len(logs) != 1 || len(traces) != 1 {
		t.Fatalf("logs/traces = %v/%v", logs, traces)
	}
	if rec.Body.Len() > 3000 {
		t.Fatalf("service response is too large: %d bytes", rec.Body.Len())
	}

	unknown := get(t, handler, "/api/v1/context/services/missing")
	if unknown.Code != http.StatusOK {
		t.Fatalf("unknown service status = %d", unknown.Code)
	}
	unknownBody := decodeMap(t, unknown)
	if unknownBody["known"] != false {
		t.Fatalf("unknown service = %v", unknownBody)
	}
}

func TestServiceLimitDoesNotMutateCatalog(t *testing.T) {
	metrics := make(map[string]Metric, 12)
	names := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("service_metric_%02d", i)
		names = append(names, name)
		metrics[name] = Metric{
			Description: "Demo metric", Type: "gauge", Unit: "1",
			Role: "resource", Direction: "neutral",
		}
	}
	catalog := &Catalog{
		Schema: SchemaVersion, Version: "v1", Metrics: metrics,
		Services: map[string]Service{
			"checkout": {Description: "Checkout", Metrics: names},
		},
	}
	handler := apiFor(t, catalog)
	first := get(t, handler, "/api/v1/context/services/checkout?limit=5")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	if got := len(decodeMap(t, first)["metrics"].([]any)); got != 5 {
		t.Fatalf("first metrics = %d", got)
	}
	second := get(t, handler, "/api/v1/context/services/checkout?limit=10")
	if got := len(decodeMap(t, second)["metrics"].([]any)); got != 10 {
		t.Fatalf("second metrics = %d; catalog service slice was mutated", got)
	}
}

func TestLogContext(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	minimal := get(t, handler, "/api/v1/context/logs/checkout.error")
	if minimal.Code != http.StatusOK {
		t.Fatalf("status = %d", minimal.Code)
	}
	body := decodeMap(t, minimal)
	if body["known"] != true || body["event"] != "exception" {
		t.Fatalf("log = %v", body)
	}
	if _, ok := body["query"]; ok {
		t.Fatal("minimal log response contains query")
	}
	if minimal.Body.Len() > 800 {
		t.Fatalf("minimal log response is too large: %d", minimal.Body.Len())
	}
	standard := decodeMap(t, get(t, handler, "/api/v1/context/logs/checkout.error?detail=standard"))
	if standard["query"] == nil || standard["fields"] == nil || standard["runbook"] == nil {
		t.Fatalf("standard log = %v", standard)
	}
	full := decodeMap(t, get(t, handler, "/api/v1/context/logs/checkout.error?detail=full"))
	if full["caveats"] == nil || full["provenance"] == nil {
		t.Fatalf("full log = %v", full)
	}
	unknown := get(t, handler, "/api/v1/context/logs/missing")
	if unknownBody := decodeMap(t, unknown); unknownBody["known"] != false || unknownBody["flags"] == nil {
		t.Fatalf("unknown log = %v", unknownBody)
	}
}

func TestTraceContext(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	minimal := get(t, handler, "/api/v1/context/traces/checkout.get")
	if minimal.Code != http.StatusOK {
		t.Fatalf("status = %d", minimal.Code)
	}
	body := decodeMap(t, minimal)
	if body["known"] != true || body["operation"] != "GET /checkout" || body["critical_path"] != true {
		t.Fatalf("trace = %v", body)
	}
	if _, ok := body["downstream"]; ok {
		t.Fatal("minimal trace response contains downstream")
	}
	if minimal.Body.Len() > 800 {
		t.Fatalf("minimal trace response is too large: %d", minimal.Body.Len())
	}
	standard := decodeMap(t, get(t, handler, "/api/v1/context/traces/checkout.get?detail=standard"))
	if standard["query"] == nil || standard["downstream"] == nil {
		t.Fatalf("standard trace = %v", standard)
	}
	full := decodeMap(t, get(t, handler, "/api/v1/context/traces/checkout.get?detail=full"))
	if full["caveats"] == nil || full["provenance"] == nil {
		t.Fatalf("full trace = %v", full)
	}
	unknown := get(t, handler, "/api/v1/context/traces/missing")
	if unknownBody := decodeMap(t, unknown); unknownBody["known"] != false {
		t.Fatalf("unknown trace = %v", unknownBody)
	}
}

func TestLogAndTraceLists(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	logs := decodeMap(t, get(t, handler, "/api/v1/context/logs?service=checkout&limit=10"))
	if logs["total"] != float64(1) {
		t.Fatalf("logs = %v", logs)
	}
	logItem := logs["items"].([]any)[0].(map[string]any)
	if logItem["id"] != "checkout.error" || logItem["event"] != "exception" {
		t.Fatalf("log item = %v", logItem)
	}
	traces := decodeMap(t, get(t, handler, "/api/v1/context/traces?service=checkout&limit=10"))
	if traces["total"] != float64(1) {
		t.Fatalf("traces = %v", traces)
	}
	traceItem := traces["items"].([]any)[0].(map[string]any)
	if traceItem["id"] != "checkout.get" || traceItem["kind"] != "server" {
		t.Fatalf("trace item = %v", traceItem)
	}
	for _, path := range []string{
		"/api/v1/context/logs/checkout.error?include=labels",
		"/api/v1/context/traces/checkout.get?include=fields",
		"/api/v1/context/logs?limit=51",
		"/api/v1/context/traces?limit=0",
	} {
		if rec := get(t, handler, path); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
	}
}

func TestCoverageAndWellKnown(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	coverage := decodeMap(t, get(t, handler, "/api/v1/context/coverage"))
	if coverage["metrics"].(map[string]any)["documented"] != float64(2) ||
		coverage["logs"].(map[string]any)["documented"] != float64(1) ||
		coverage["traces"].(map[string]any)["documented"] != float64(1) {
		t.Fatalf("coverage = %v", coverage)
	}
	wellKnown := decodeMap(t, get(t, handler, "/.well-known/skopos.json"))
	if wellKnown["schema"] != "skopos.well-known/v1" || wellKnown["context_api"] != "/api/v1/context" {
		t.Fatalf("well-known = %v", wellKnown)
	}
	signals, _ := wellKnown["signals"].([]any)
	if len(signals) != 4 {
		t.Fatalf("signals = %v", signals)
	}
}

func TestETagCache(t *testing.T) {
	handler := apiFor(t, mustCatalog(t))
	first := get(t, handler, "/api/v1/context/metrics/demo_temperature_celsius")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if got := first.Header().Get("Cache-Control"); got != "private, max-age=300" {
		t.Fatalf("Cache-Control = %q", got)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/context/metrics/demo_temperature_celsius", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 body = %q", rec.Body)
	}
}

func TestIndex(t *testing.T) {
	rec := get(t, apiFor(t, mustCatalog(t)), "/api/v1/context")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeMap(t, rec)
	if body["schema"] != SchemaVersion || body["version"] != "2026-11-01.1" {
		t.Fatalf("index = %v", body)
	}
	levels, _ := body["detail_levels"].([]any)
	if len(levels) != 3 {
		t.Fatalf("detail levels = %v", levels)
	}
}

func TestEmptyCatalogAPIIsHonest(t *testing.T) {
	handler := apiFor(t, EmptyCatalog())
	list := decodeMap(t, get(t, handler, "/api/v1/context/metrics"))
	items, _ := list["items"].([]any)
	if len(items) != 0 || list["total"] != float64(0) {
		t.Fatalf("empty list = %v", list)
	}
	metric := decodeMap(t, get(t, handler, "/api/v1/context/metrics/anything"))
	if metric["known"] != false {
		t.Fatalf("empty metric = %v", metric)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/context/metrics/demo_temperature_celsius", strings.NewReader("{}"))
	apiFor(t, mustCatalog(t)).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}
