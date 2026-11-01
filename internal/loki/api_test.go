package loki

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeFetcher struct {
	entries []LogEntry
}

func (f fakeFetcher) FetchRange(context.Context, time.Time, time.Time) ([]LogEntry, error) {
	return f.entries, nil
}

func testAPI(t *testing.T) (http.Handler, []LogEntry) {
	t.Helper()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	entries := []LogEntry{
		{
			Timestamp: base,
			Body:      "request timeout after 30s",
			Labels:    map[string]string{"service_name": "checkout", "severity_text": "ERROR"},
		},
		{
			Timestamp: base.Add(time.Second),
			Body:      "request ok",
			Labels:    map[string]string{"service_name": "checkout", "severity_text": "INFO"},
		},
	}
	api := NewAPI(fakeFetcher{entries: entries}, "test-version")
	api.Now = func() time.Time { return base.Add(time.Minute) }
	mux := http.NewServeMux()
	api.Register(mux)
	return mux, entries
}

func do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestQueryRangeStreams(t *testing.T) {
	h, _ := testAPI(t)
	query := url.QueryEscape(`{service_name="checkout"} |= "timeout"`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200000000000&end=1793494800000000000&limit=10`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	data := body["data"].(map[string]any)
	if data["resultType"] != "streams" {
		t.Fatalf("resultType = %v", data["resultType"])
	}
	result := data["result"].([]any)
	if len(result) != 1 {
		t.Fatalf("result = %#v", result)
	}
	stream := result[0].(map[string]any)
	values := stream["values"].([]any)
	if len(values) != 1 || values[0].([]any)[1] != "request timeout after 30s" {
		t.Fatalf("values = %#v", values)
	}
}

func TestLabelsValuesAndSeries(t *testing.T) {
	h, _ := testAPI(t)
	rec := do(t, h, `/loki/api/v1/labels?start=1793491200000000000&end=1793494800000000000`)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	labels := body["data"].([]any)
	if len(labels) != 2 || labels[0] != "service_name" {
		t.Fatalf("labels = %#v", labels)
	}

	rec = do(t, h, `/loki/api/v1/label/service_name/values`)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	values := body["data"].([]any)
	if len(values) != 1 || values[0] != "checkout" {
		t.Fatalf("values = %#v", values)
	}

	rec = do(t, h, `/loki/api/v1/series?match[]=`+url.QueryEscape(`{service_name="checkout"}`))
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	series := body["data"].([]any)
	if len(series) != 2 {
		t.Fatalf("series = %#v", series)
	}
}

func TestSeriesPOST(t *testing.T) {
	h, _ := testAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/series", strings.NewReader("match[]="+url.QueryEscape(`{service_name="checkout"}`)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST series = %d %s", rec.Code, rec.Body)
	}
}

func TestQueryParameterFiltersLabelsAndInstantTime(t *testing.T) {
	h, _ := testAPI(t)
	rec := do(t, h, `/loki/api/v1/labels?query=`+url.QueryEscape(`{severity_text="ERROR"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered labels = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, `/loki/api/v1/query?query=`+url.QueryEscape(`{service_name="checkout"}`)+`&time=2026-11-01T00:00:00Z`)
	if rec.Code != http.StatusOK {
		t.Fatalf("instant query = %d %s", rec.Code, rec.Body)
	}
}

func TestBadQueryAndBuildInfo(t *testing.T) {
	h, _ := testAPI(t)
	rec := do(t, h, `/loki/api/v1/query?query=`+url.QueryEscape(`{service_name="checkout"} | json`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"status":"error"`) {
		t.Fatalf("bad query = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, `/loki/api/v1/series`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("series without match = %d", rec.Code)
	}
	rec = do(t, h, `/loki/api/v1/status/buildinfo`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"test-version"`) {
		t.Fatalf("buildinfo = %d %s", rec.Code, rec.Body)
	}
}
