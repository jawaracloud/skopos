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
	rec := do(t, h, `/loki/api/v1/query?query=`+url.QueryEscape(`{service_name="checkout"} | pattern "<_>"`))
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

func TestParserStageQueryEndpoint(t *testing.T) {
	h, _ := metricAPI(t)
	query := url.QueryEscape(`{service_name="checkout"} | json | level="error"`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200000000000&end=1793491500000000000`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.ResultType != "streams" {
		t.Fatalf("resultType = %q", body.Data.ResultType)
	}
	if len(body.Data.Result) != 2 {
		t.Fatalf("result = %#v", body.Data.Result)
	}
	for _, stream := range body.Data.Result {
		if stream.Stream["level"] != "error" {
			t.Fatalf("stream labels = %#v", stream.Stream)
		}
		if len(stream.Values) != 1 {
			t.Fatalf("values = %#v", stream.Values)
		}
	}
}

func TestMetricQueryRangeCountOverTime(t *testing.T) {
	h, _ := metricAPI(t)
	query := url.QueryEscape(`count_over_time({service_name="checkout"}[5m])`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200&end=1793491500&step=60`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.ResultType != "matrix" {
		t.Fatalf("resultType = %q", body.Data.ResultType)
	}
	if len(body.Data.Result) != 2 {
		t.Fatalf("result = %#v", body.Data.Result)
	}
	want := map[string][][2]any{
		"ERROR": {
			{float64(1793491260), "1"},
			{float64(1793491320), "1"},
			{float64(1793491380), "1"},
			{float64(1793491440), "2"},
			{float64(1793491500), "2"},
		},
		"INFO": {
			{float64(1793491260), "1"},
			{float64(1793491320), "1"},
			{float64(1793491380), "1"},
			{float64(1793491440), "1"},
			{float64(1793491500), "1"},
		},
	}
	for _, series := range body.Data.Result {
		expected := want[series.Metric["severity_text"]]
		if len(expected) == 0 {
			t.Fatalf("unexpected metric %#v", series.Metric)
		}
		if len(series.Values) != len(expected) {
			t.Fatalf("%s values = %#v", series.Metric["severity_text"], series.Values)
		}
		for i, pair := range series.Values {
			if pair[0] != expected[i][0] || pair[1] != expected[i][1] {
				t.Fatalf("%s value %d = %#v, want %#v", series.Metric["severity_text"], i, pair, expected[i])
			}
		}
	}
}

func TestMetricQueryRangeRate(t *testing.T) {
	h, _ := metricAPI(t)
	query := url.QueryEscape(`rate({service_name="checkout", severity_text="ERROR"}[5m])`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491260&end=1793491320&step=60`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			Result []struct {
				Values [][]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data.Result) != 1 || len(body.Data.Result[0].Values) != 2 {
		t.Fatalf("result = %#v", body.Data.Result)
	}
	first := body.Data.Result[0].Values[0]
	if first[1] != "0.0033333333333333335" {
		t.Fatalf("rate value = %#v", first)
	}
}

func TestMetricSumByParsedLabel(t *testing.T) {
	h, _ := metricAPI(t)
	query := url.QueryEscape(`sum by (level) (count_over_time({service_name="checkout"} | json [5m]))`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200&end=1793491500&step=300`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data.Result) != 2 {
		t.Fatalf("result = %#v", body.Data.Result)
	}
	got := make(map[string]string)
	for _, series := range body.Data.Result {
		if len(series.Metric) != 1 {
			t.Fatalf("sum by labels = %#v", series.Metric)
		}
		got[series.Metric["level"]] = series.Values[0][1].(string)
	}
	if got["error"] != "2" || got["info"] != "1" {
		t.Fatalf("sum by values = %#v", got)
	}
}

func TestMetricInstantVector(t *testing.T) {
	h, _ := metricAPI(t)
	query := url.QueryEscape(`count_over_time({service_name="checkout"}[5m])`)
	rec := do(t, h, `/loki/api/v1/query?query=`+query+`&time=1793491500`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.ResultType != "vector" || len(body.Data.Result) != 2 {
		t.Fatalf("data = %#v", body.Data)
	}
	for _, series := range body.Data.Result {
		if series.Value[0] != float64(1793491500) {
			t.Fatalf("value timestamp = %#v", series.Value)
		}
	}
}

func TestMetricRequiresStepAndRejectsExcessSamples(t *testing.T) {
	h, _ := metricAPI(t)
	query := url.QueryEscape(`count_over_time({service_name="checkout"}[5m])`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200&end=1793491500`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing step = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200&end=1793491500&step=0`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("zero step = %d %s", rec.Code, rec.Body)
	}

	mux := http.NewServeMux()
	api := NewAPI(fakeFetcher{entries: metricEntries()}, "test-version")
	api.MaxSamples = 2
	api.Register(mux)
	rec = do(t, mux, `/loki/api/v1/query_range?query=`+query+`&start=1793491200&end=1793491500&step=60`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sample limit") {
		t.Fatalf("sample limit = %d %s", rec.Code, rec.Body)
	}
}

func TestLogDirectionAndLimit(t *testing.T) {
	h, _ := testAPI(t)
	query := url.QueryEscape(`{service_name="checkout"}`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200000000000&end=1793491500000000000&direction=forward&limit=1`)
	var body struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data.Result) != 1 || len(body.Data.Result[0].Values) != 1 {
		t.Fatalf("forward result = %#v", body.Data.Result)
	}
	if body.Data.Result[0].Values[0][1] != "request timeout after 30s" {
		t.Fatalf("forward first value = %#v", body.Data.Result[0].Values)
	}

	rec = do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200000000000&end=1793491500000000000&limit=1`)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if got := body.Data.Result[0].Values[0][1]; got != "request ok" {
		t.Fatalf("backward first value = %q", got)
	}
}

func TestSelectorMissingLabelMatchesEmpty(t *testing.T) {
	h, _ := testAPI(t)
	query := url.QueryEscape(`{service_name="checkout", missing=""}`)
	rec := do(t, h, `/loki/api/v1/query_range?query=`+query+`&start=1793491200000000000&end=1793491500000000000`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resultType":"streams"`) {
		t.Fatalf("missing label selector = %d %s", rec.Code, rec.Body)
	}
}

func TestLabelsAndSeriesRejectMetricQueries(t *testing.T) {
	h, _ := testAPI(t)
	metric := url.QueryEscape(`count_over_time({service_name="checkout"}[5m])`)
	for _, path := range []string{
		`/loki/api/v1/labels?query=` + metric,
		`/loki/api/v1/series?match[]=` + metric,
		`/loki/api/v1/label/level/values?query=` + metric,
	} {
		rec := do(t, h, path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestLabelValuesAndSeriesQueryFilter(t *testing.T) {
	h, _ := testAPI(t)
	rec := do(t, h, `/loki/api/v1/label/severity_text/values?query=`+url.QueryEscape(`{service_name="checkout", severity_text="ERROR"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ERROR"`) || strings.Contains(rec.Body.String(), `"INFO"`) {
		t.Fatalf("filtered values = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, `/loki/api/v1/series?query=`+url.QueryEscape(`{severity_text="ERROR"}`))
	if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), `"service_name"`) != 1 {
		t.Fatalf("filtered series = %d %s", rec.Code, rec.Body)
	}
}

func TestLabelsValuesAndSeriesUseParserLabels(t *testing.T) {
	h, _ := metricAPI(t)
	query := url.QueryEscape(`{service_name="checkout"} | json`)
	rec := do(t, h, `/loki/api/v1/labels?query=`+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("labels = %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{`"level"`, `"status"`, `"msg"`} {
		if !strings.Contains(rec.Body.String(), name) {
			t.Fatalf("labels missing %s: %s", name, rec.Body)
		}
	}
	rec = do(t, h, `/loki/api/v1/label/level/values?query=`+query)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"error"`) || !strings.Contains(rec.Body.String(), `"info"`) {
		t.Fatalf("label values = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, `/loki/api/v1/series?query=`+query)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"level":"error"`) {
		t.Fatalf("series = %d %s", rec.Code, rec.Body)
	}
}

func TestEvaluationPointsAlignToUnixStep(t *testing.T) {
	base := time.Unix(1793491200, 0).UTC()
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		step  time.Duration
		want  []int64
	}{
		{
			name:  "start rounds down",
			start: base.Add(90 * time.Second),
			end:   base.Add(5 * time.Minute),
			step:  time.Minute,
			want:  []int64{1793491260, 1793491320, 1793491380, 1793491440, 1793491500},
		},
		{
			name:  "end rounds up",
			start: base,
			end:   base.Add(5*time.Minute + 30*time.Second),
			step:  time.Minute,
			want:  []int64{1793491200, 1793491260, 1793491320, 1793491380, 1793491440, 1793491500, 1793491560},
		},
		{
			name:  "five minute step",
			start: base.Add(time.Minute),
			end:   base.Add(6 * time.Minute),
			step:  5 * time.Minute,
			want:  []int64{1793491200, 1793491500, 1793491800},
		},
	}
	for _, tc := range tests {
		points, err := evaluationPoints(tc.start, tc.end, tc.step, defaultMaxSamples)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(points) != len(tc.want) {
			t.Fatalf("%s: %d points, want %d: %#v", tc.name, len(points), len(tc.want), points)
		}
		for i, want := range tc.want {
			if points[i].Unix() != want {
				t.Fatalf("%s: point %d = %d, want %d", tc.name, i, points[i].Unix(), want)
			}
		}
	}
}

func metricEntries() []LogEntry {
	base := time.Unix(1793491200, 0).UTC()
	return []LogEntry{
		{
			Timestamp: base.Add(30 * time.Second),
			Body:      `{"level":"error","status":500,"msg":"timeout"}`,
			Labels:    map[string]string{"service_name": "checkout", "severity_text": "ERROR"},
		},
		{
			Timestamp: base.Add(210 * time.Second),
			Body:      `{"level":"error","status":503,"msg":"still broken"}`,
			Labels:    map[string]string{"service_name": "checkout", "severity_text": "ERROR"},
		},
		{
			Timestamp: base.Add(60 * time.Second),
			Body:      `{"level":"info","status":200,"msg":"ok"}`,
			Labels:    map[string]string{"service_name": "checkout", "severity_text": "INFO"},
		},
		{
			Timestamp: base.Add(330 * time.Second),
			Body:      `{"level":"info","status":200,"msg":"ok again"}`,
			Labels:    map[string]string{"service_name": "checkout", "severity_text": "INFO"},
		},
		{
			Timestamp: base.Add(90 * time.Second),
			Body:      `{"level":"error","status":501,"msg":"cart down"}`,
			Labels:    map[string]string{"service_name": "cart", "severity_text": "ERROR"},
		},
	}
}

func metricAPI(t *testing.T) (http.Handler, []LogEntry) {
	t.Helper()
	entries := metricEntries()
	api := NewAPI(fakeFetcher{entries: entries}, "test-version")
	api.Now = func() time.Time { return time.Unix(1793491800, 0).UTC() }
	mux := http.NewServeMux()
	api.Register(mux)
	return mux, entries
}
