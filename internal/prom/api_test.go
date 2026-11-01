package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeQueryEngine struct {
	instant Value
	matrix  Matrix
	err     error
}

func (f fakeQueryEngine) Query(context.Context, string, time.Time) (Value, error) {
	return f.instant, f.err
}

func (f fakeQueryEngine) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) (Matrix, error) {
	return f.matrix, f.err
}

type fakeSeriesFetcher struct {
	series   []Series
	metadata map[string]Metadata
	err      error
}

func (f fakeSeriesFetcher) Fetch(context.Context, Selector, time.Time, time.Time) ([]Series, error) {
	return f.series, f.err
}

func (f fakeSeriesFetcher) Metadata(context.Context, string) (map[string]Metadata, error) {
	return f.metadata, f.err
}

func testAPI(engine QueryEngine, fetcher SeriesFetcher) http.Handler {
	api := NewAPI(engine, fetcher, "test-version")
	api.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	api.Register(mux)
	return mux
}

func do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeSuccess(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "success" {
		t.Fatalf("status body = %#v", body)
	}
	data, _ := body["data"].(map[string]any)
	return data
}

func TestQueryVector(t *testing.T) {
	engine := fakeQueryEngine{instant: Vector{{
		Labels: map[string]string{"__name__": "http_requests_total", "service_name": "checkout"},
		T:      time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		V:      6,
	}}}
	h := testAPI(engine, fakeSeriesFetcher{})
	rec := do(t, h, `/api/v1/query?query=http_requests_total&time=1793491200`)
	data := decodeSuccess(t, rec)
	if data["resultType"] != "vector" {
		t.Fatalf("resultType = %v", data["resultType"])
	}
	result := data["result"].([]any)
	if len(result) != 1 {
		t.Fatalf("result = %#v", result)
	}
	sample := result[0].(map[string]any)
	metric := sample["metric"].(map[string]any)
	if metric["service_name"] != "checkout" {
		t.Fatalf("metric = %#v", metric)
	}
	value := sample["value"].([]any)
	if value[1] != "6" {
		t.Fatalf("value = %#v", value)
	}
}

func TestQueryRangeMatrix(t *testing.T) {
	engine := fakeQueryEngine{matrix: Matrix{{
		Labels: map[string]string{"__name__": "http_requests_total"},
		Samples: []Sample{
			{T: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), V: 1},
			{T: time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC).UnixMilli(), V: 2},
		},
	}}}
	h := testAPI(engine, fakeSeriesFetcher{})
	rec := do(t, h, `/api/v1/query_range?query=http_requests_total&start=1793491200&end=1793491260&step=60`)
	data := decodeSuccess(t, rec)
	if data["resultType"] != "matrix" {
		t.Fatalf("resultType = %v", data["resultType"])
	}
	result := data["result"].([]any)
	series := result[0].(map[string]any)
	values := series["values"].([]any)
	if len(values) != 2 {
		t.Fatalf("values = %#v", values)
	}
}

func TestSeriesAndLabels(t *testing.T) {
	fetcher := fakeSeriesFetcher{series: []Series{
		{Labels: map[string]string{"__name__": "http_requests_total", "service_name": "checkout"}},
		{Labels: map[string]string{"__name__": "http_requests_total", "service_name": "cart"}},
	}}
	h := testAPI(fakeQueryEngine{}, fetcher)

	rec := do(t, h, `/api/v1/series?match[]=http_requests_total`)
	if rec.Code != http.StatusOK {
		t.Fatalf("series status = %d body=%s", rec.Code, rec.Body)
	}
	var seriesBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &seriesBody)
	seriesData := seriesBody["data"].([]any)
	if len(seriesData) != 2 {
		t.Fatalf("series data = %#v", seriesData)
	}

	rec = do(t, h, `/api/v1/labels?match[]=http_requests_total`)
	var labelsBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &labelsBody)
	labels := labelsBody["data"].([]any)
	if len(labels) != 2 || labels[0] != "__name__" {
		t.Fatalf("labels = %#v", labels)
	}

	rec = do(t, h, `/api/v1/label/service_name/values?match[]=http_requests_total`)
	var valuesBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &valuesBody)
	values := valuesBody["data"].([]any)
	if len(values) != 2 || values[0] != "cart" || values[1] != "checkout" {
		t.Fatalf("values = %#v", values)
	}
}

func TestSeriesPOST(t *testing.T) {
	fetcher := fakeSeriesFetcher{series: []Series{
		{Labels: map[string]string{"__name__": "http_requests_total", "service_name": "checkout"}},
	}}
	h := testAPI(fakeQueryEngine{}, fetcher)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/series", strings.NewReader("match[]=http_requests_total"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST series = %d %s", rec.Code, rec.Body)
	}
}

func TestMetadataAndBuildInfo(t *testing.T) {
	fetcher := fakeSeriesFetcher{metadata: map[string]Metadata{
		"http_requests_total": {Type: "counter", Help: "requests", Unit: "1"},
	}}
	h := testAPI(fakeQueryEngine{}, fetcher)
	rec := do(t, h, `/api/v1/metadata`)
	data := decodeSuccess(t, rec)
	item := data["http_requests_total"].([]any)[0].(map[string]any)
	if item["type"] != "counter" || item["help"] != "requests" {
		t.Fatalf("metadata = %#v", item)
	}

	rec = do(t, h, `/api/v1/status/buildinfo`)
	data = decodeSuccess(t, rec)
	if data["version"] != "test-version" {
		t.Fatalf("buildinfo = %#v", data)
	}
}

func TestBadRequestsAndExecutionErrors(t *testing.T) {
	h := testAPI(fakeQueryEngine{}, fakeSeriesFetcher{})
	for _, path := range []string{
		`/api/v1/query`,
		`/api/v1/query_range?query=x&start=1`,
		`/api/v1/query_range?query=x&start=1&end=2&step=0`,
		`/api/v1/series`,
	} {
		rec := do(t, h, path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, body=%s", path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"status":"error"`) {
			t.Fatalf("%s body = %s", path, rec.Body)
		}
	}

	h = testAPI(fakeQueryEngine{err: errors.New("backend down")}, fakeSeriesFetcher{})
	rec := do(t, h, `/api/v1/query?query=x`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "backend down") {
		t.Fatalf("execution error = %d %s", rec.Code, rec.Body)
	}

	h = testAPI(fakeQueryEngine{err: fmt.Errorf("%w: parse query: bad", ErrBadQuery)}, fakeSeriesFetcher{})
	rec = do(t, h, `/api/v1/query?query=x`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_data") {
		t.Fatalf("bad query = %d %s", rec.Code, rec.Body)
	}
}
