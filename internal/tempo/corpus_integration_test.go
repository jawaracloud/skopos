//go:build integration

package tempo

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

	"github.com/jawaracloud/skopos/internal/chclient"
	"github.com/jawaracloud/skopos/internal/plan"
	corpus "github.com/jawaracloud/skopos/test/corpus/traceql"
)

func TestIntegrationTraceQLCorpus(t *testing.T) {
	baseURL := os.Getenv("CLICKHOUSE_URL")
	if baseURL == "" {
		t.Fatal("CLICKHOUSE_URL is required for integration tests")
	}
	c, err := corpus.Load()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	base := c.BaseTime()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "skopos_traceql_" + suffix
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
	exec("CREATE TABLE `" + database + "`.`otel_traces` (" +
		"Timestamp DateTime64(9), TraceId String, SpanId String, ParentSpanId String, TraceState String, " +
		"SpanName LowCardinality(String), SpanKind LowCardinality(String), ServiceName LowCardinality(String), " +
		"ResourceAttributes Map(String, String), ScopeName String, ScopeVersion String, SpanAttributes Map(String, String), " +
		"Duration UInt64, StatusCode LowCardinality(String), StatusMessage String, " +
		"Events Nested (Timestamp DateTime64(9), Name LowCardinality(String), Attributes Map(String, String)), " +
		"Links Nested (TraceId String, SpanId String, TraceState String, Attributes Map(String, String))" +
		") ENGINE = MergeTree ORDER BY (ServiceName, SpanName, Timestamp)")

	rows, err := c.ClickHouseRows(base)
	if err != nil {
		t.Fatalf("render ClickHouse rows: %v", err)
	}
	exec("INSERT INTO `" + database + "`.`otel_traces` FORMAT JSONEachRow\n" + strings.Join(rows, "\n"))

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
	api := NewAPI(fetcher, "integration")
	api.Now = func() time.Time { return base.Add(10 * time.Minute) }
	mux := http.NewServeMux()
	api.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	getJSON := func(path string, out any) *http.Response {
		t.Helper()
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
		}
		return resp
	}

	start := base.Add(-time.Minute).Unix()
	end := base.Add(time.Hour).Unix()
	for _, test := range c.Queries {
		t.Run(test.Name, func(t *testing.T) {
			values := url.Values{}
			values.Set("q", test.Query)
			values.Set("start", fmt.Sprintf("%d", start))
			values.Set("end", fmt.Sprintf("%d", end))
			values.Set("limit", "100")
			values.Set("spss", "100")
			resp, err := http.Get(server.URL + "/api/search?" + values.Encode())
			if err != nil {
				t.Fatalf("GET search: %v", err)
			}
			defer resp.Body.Close()
			if test.ExpectError {
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
				}
				var payload map[string]string
				if err := json.Unmarshal(body, &payload); err != nil || payload["error"] == "" {
					t.Fatalf("error body = %s", body)
				}
				if test.ExpectErrorContains != "" && !strings.Contains(payload["error"], test.ExpectErrorContains) {
					t.Fatalf("error = %q, want it to contain %q", payload["error"], test.ExpectErrorContains)
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d: %s", resp.StatusCode, body)
			}
			var body corpusSearchResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			ids := make([]string, 0, len(body.Traces))
			matched := make(map[string][]string)
			for _, trace := range body.Traces {
				ids = append(ids, trace.TraceID)
				for _, set := range trace.SpanSets {
					for _, span := range set.Spans {
						matched[trace.TraceID] = append(matched[trace.TraceID], span.SpanID)
					}
				}
			}
			if !equalStrings(ids, test.ExpectTraceIDs) {
				t.Fatalf("trace IDs = %v, want %v", sortedSet(ids), sortedSet(test.ExpectTraceIDs))
			}
			for id, want := range test.ExpectMatchedSpans {
				if !equalStrings(matched[id], want) {
					t.Fatalf("matched spans for %s = %v, want %v", id, sortedSet(matched[id]), sortedSet(want))
				}
			}
		})
	}

	for _, test := range c.TraceTests {
		t.Run("trace/"+test.TraceID, func(t *testing.T) {
			var body map[string]any
			getJSON("/api/traces/"+test.TraceID, &body)
			resourceSpans := body["resourceSpans"].([]any)
			count := 0
			for _, rs := range resourceSpans {
				for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
					count += len(ss.(map[string]any)["spans"].([]any))
				}
			}
			if count != test.SpanCount {
				t.Fatalf("v1 span count = %d, want %d", count, test.SpanCount)
			}

			req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v2/traces/"+test.TraceID, nil)
			req.Header.Set("Accept", "application/protobuf")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("v2 protobuf request: %v", err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("v2 protobuf = %d: %s", resp.StatusCode, data)
			}
			checkTraceTest(t, test, flattenOTLP(t, decodeV2Envelope(t, data)))
		})
	}

	for _, test := range c.TagValueTests {
		t.Run("tag/"+test.Name, func(t *testing.T) {
			path := "/api/v2/search/tag/" + url.PathEscape(test.Tag) + "/values?start=" + fmt.Sprintf("%d", start) + "&end=" + fmt.Sprintf("%d", end)
			var body struct {
				TagValues []struct {
					Value string `json:"value"`
				} `json:"tagValues"`
			}
			getJSON(path, &body)
			values := make([]string, 0, len(body.TagValues))
			for _, value := range body.TagValues {
				values = append(values, value.Value)
			}
			if !equalStrings(values, test.Expect) {
				t.Fatalf("values = %v, want %v", sortedSet(values), sortedSet(test.Expect))
			}
		})
	}
}
