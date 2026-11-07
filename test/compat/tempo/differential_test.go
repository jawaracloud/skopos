// Package tempocompat runs the gateway Tempo API and a real Grafana Tempo
// container against the same OTLP corpus and compares search, trace-by-id, and
// tag behavior. It does not import any Tempo code; the container is the
// reference implementation.
package tempocompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/tempo"
	corpus "github.com/jawaracloud/skopos/test/corpus/traceql"
)

func TestDifferential(t *testing.T) {
	if os.Getenv("TEMPO_COMPAT") != "1" {
		t.Skip("set TEMPO_COMPAT=1 to run the Tempo differential harness")
	}
	docker := os.Getenv("DOCKER")
	if docker == "" {
		docker = "docker"
	}
	if _, err := exec.LookPath(docker); err != nil {
		t.Skipf("%s is not available: %v", docker, err)
	}
	c, err := corpus.Load()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	base := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	image := os.Getenv("TEMPO_IMAGE")
	if image == "" {
		image = "grafana/tempo:2.7.2"
	}

	tempoURL, otlpURL := startTempo(t, docker, image, base)
	ingest(t, otlpURL, c, base)
	start := base.Add(-time.Minute).Unix()
	end := base.Add(10 * time.Minute).Unix()
	waitForTraces(t, tempoURL, len(c.Traces), start, end)

	gateway := newGateway(t, c, base)

	runSearchComparisons(t, c, tempoURL, gateway, start, end)
	runTagComparisons(t, c, tempoURL, gateway, start, end)
	runTraceComparisons(t, c, tempoURL, gateway)
}

func startTempo(t *testing.T, docker, image string, base time.Time) (string, string) {
	t.Helper()
	name := fmt.Sprintf("skopos-tempo-compat-%d", os.Getpid())
	httpPort := freePort(t)
	otlpPort := freePort(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "tempo.yaml")
	configBody := `server:
  http_listen_port: 3200
  log_level: warn
distributor:
  receivers:
    otlp:
      protocols:
        http:
          endpoint: 0.0.0.0:4318
        grpc:
          endpoint: 0.0.0.0:4317
compactor:
  disabled: true
ingester:
  max_block_duration: 1h
  trace_idle_period: 1h
  complete_block_timeout: 1h
storage:
  trace:
    backend: local
    local:
      path: /var/tempo/traces
    wal:
      path: /var/tempo/wal
`
	if err := os.WriteFile(config, []byte(configBody), 0o644); err != nil {
		t.Fatalf("write tempo config: %v", err)
	}
	args := []string{
		"run", "--rm", "-d", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:3200", httpPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:4318", otlpPort),
		"-v", config + ":/etc/tempo.yaml:ro",
		image, "-config.file=/etc/tempo.yaml",
	}
	out, err := exec.Command(docker, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run tempo: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(docker, "rm", "-f", name).Run()
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL, fmt.Sprintf("http://127.0.0.1:%d", otlpPort)
			}
		}
		time.Sleep(time.Second)
	}
	logs, _ := exec.Command(docker, "logs", name).CombinedOutput()
	t.Fatalf("tempo did not become ready at %s\n%s", baseURL, logs)
	return "", ""
}

func waitForTraces(t *testing.T, tempoURL string, want int, start, end int64) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		status, body, raw := search(t, tempoURL, "{ }", start, end)
		if status == http.StatusOK && len(body.Traces) >= want {
			return
		}
		last = raw
		time.Sleep(time.Second)
	}
	t.Fatalf("tempo did not expose %d traces within 45s; last response: %s", want, last)
}

func ingest(t *testing.T, otlpURL string, c *corpus.Corpus, base time.Time) {
	t.Helper()
	body, err := c.OTLPJSON(base)
	if err != nil {
		t.Fatalf("render OTLP JSON: %v", err)
	}
	resp, err := http.Post(otlpURL+"/v1/traces", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("push OTLP to tempo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("tempo OTLP push = %d: %s", resp.StatusCode, data)
	}
	time.Sleep(2 * time.Second)
}

type searchTrace struct {
	TraceID  string `json:"traceID"`
	SpanSets []struct {
		Spans []struct {
			SpanID string `json:"spanID"`
		} `json:"spans"`
		Matched int `json:"matched"`
	} `json:"spanSets"`
}

type searchResponse struct {
	Traces []searchTrace `json:"traces"`
}

func search(t *testing.T, baseURL, query string, start, end int64) (int, searchResponse, string) {
	t.Helper()
	values := url.Values{}
	values.Set("q", query)
	values.Set("start", strconv.FormatInt(start, 10))
	values.Set("end", strconv.FormatInt(end, 10))
	values.Set("limit", "100")
	values.Set("spss", "100")
	resp, err := http.Get(baseURL + "/api/search?" + values.Encode())
	if err != nil {
		t.Fatalf("GET search %q: %v", query, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var body searchResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("decode search %q: %v (%s)", query, err, data)
		}
	}
	return resp.StatusCode, body, string(data)
}

func runSearchComparisons(t *testing.T, c *corpus.Corpus, tempoURL string, gateway http.Handler, start, end int64) {
	t.Helper()
	for _, test := range c.Queries {
		t.Run("search/"+test.Name, func(t *testing.T) {
			if test.ExpectError {
				gwStatus, _, gwBody := gatewaySearch(t, gateway, test.Query, start, end)
				if gwStatus != http.StatusBadRequest {
					t.Fatalf("gateway status = %d, want 400: %s", gwStatus, gwBody)
				}
				if test.ExpectErrorContains != "" {
					var body map[string]string
					if err := json.Unmarshal([]byte(gwBody), &body); err != nil || !strings.Contains(body["error"], test.ExpectErrorContains) {
						t.Fatalf("gateway error = %q, want it to contain %q", body["error"], test.ExpectErrorContains)
					}
				}
				return
			}
			gwStatus, gw, gwBody := gatewaySearch(t, gateway, test.Query, start, end)
			if gwStatus != http.StatusOK {
				t.Fatalf("gateway status = %d: %s", gwStatus, gwBody)
			}
			gwIDs, gwMatched := summarizeSearch(gw)
			wantIDs := sorted(test.ExpectTraceIDs)
			if !equal(gwIDs, wantIDs) {
				t.Fatalf("gateway trace IDs = %v, want %v", gwIDs, wantIDs)
			}
			if len(test.ExpectMatchedSpans) > 0 {
				for id, want := range test.ExpectMatchedSpans {
					if !equal(gwMatched[id], sorted(want)) {
						t.Fatalf("gateway matched spans %s = %v, want %v", id, gwMatched[id], sorted(want))
					}
				}
			}
			if test.SkipTempo {
				return
			}
			tempoStatus, tempoBody, tempoRaw := search(t, tempoURL, test.Query, start, end)
			if tempoStatus != http.StatusOK {
				t.Fatalf("tempo status = %d: %s", tempoStatus, tempoRaw)
			}
			tempoIDs, tempoMatched := summarizeSearch(tempoBody)
			if !equal(tempoIDs, wantIDs) {
				t.Fatalf("corpus/reference mismatch: tempo trace IDs = %v, want %v (query %s)", tempoIDs, wantIDs, test.Query)
			}
			if len(test.ExpectMatchedSpans) > 0 {
				for id, want := range test.ExpectMatchedSpans {
					if !equal(tempoMatched[id], sorted(want)) {
						t.Fatalf("corpus/reference mismatch: tempo matched spans %s = %v, want %v", id, tempoMatched[id], sorted(want))
					}
				}
			}
		})
	}
}

func gatewaySearch(t *testing.T, h http.Handler, query string, start, end int64) (int, searchResponse, string) {
	t.Helper()
	values := url.Values{}
	values.Set("q", query)
	values.Set("start", strconv.FormatInt(start, 10))
	values.Set("end", strconv.FormatInt(end, 10))
	values.Set("limit", "100")
	values.Set("spss", "100")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/search?"+values.Encode(), nil))
	var body searchResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode gateway search: %v (%s)", err, rec.Body)
		}
	}
	return rec.Code, body, rec.Body.String()
}

func summarizeSearch(body searchResponse) ([]string, map[string][]string) {
	ids := make([]string, 0, len(body.Traces))
	matched := make(map[string][]string)
	for _, trace := range body.Traces {
		ids = append(ids, strings.ToLower(trace.TraceID))
		for _, set := range trace.SpanSets {
			for _, span := range set.Spans {
				matched[strings.ToLower(trace.TraceID)] = append(matched[strings.ToLower(trace.TraceID)], strings.ToLower(span.SpanID))
			}
		}
	}
	sort.Strings(ids)
	for id := range matched {
		sort.Strings(matched[id])
	}
	return ids, matched
}

func runTagComparisons(t *testing.T, c *corpus.Corpus, tempoURL string, gateway http.Handler, start, end int64) {
	t.Helper()
	query := url.Values{}
	query.Set("start", strconv.FormatInt(start, 10))
	query.Set("end", strconv.FormatInt(end, 10))

	tempoScopes := httpJSON(t, tempoURL+"/api/v2/search/tags?"+query.Encode())
	gwRec := httptest.NewRecorder()
	gateway.ServeHTTP(gwRec, httptest.NewRequest(http.MethodGet, "/api/v2/search/tags?"+query.Encode(), nil))
	if gwRec.Code != http.StatusOK {
		t.Fatalf("gateway tags = %d: %s", gwRec.Code, gwRec.Body)
	}
	gwScopes := decodeJSON(t, gwRec.Body.Bytes())
	if !equalScopeMaps(tempoScopes, gwScopes) {
		t.Fatalf("tag scopes differ:\ntempo:   %v\ngateway: %v", tempoScopes, gwScopes)
	}

	for _, test := range c.TagNameTests {
		if got := tempoScopes[test.Scope]; !equal(got, sorted(test.Expect)) {
			t.Fatalf("corpus/reference mismatch: tempo scope %s = %v, want %v", test.Scope, got, sorted(test.Expect))
		}
	}

	for _, test := range c.TagValueTests {
		if test.SkipTempo {
			continue
		}
		path := "/api/v2/search/tag/" + url.PathEscape(test.Tag) + "/values?" + query.Encode()
		tempoValues := tagValues(t, tempoURL+path)
		rec := httptest.NewRecorder()
		gateway.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("gateway tag values %s = %d: %s", test.Tag, rec.Code, rec.Body)
		}
		gwValues := decodeTagValues(t, rec.Body.Bytes())
		if !equal(tempoValues, sorted(test.Expect)) {
			t.Fatalf("corpus/reference mismatch: tempo values for %s = %v, want %v", test.Tag, tempoValues, sorted(test.Expect))
		}
		if !equal(gwValues, tempoValues) {
			t.Fatalf("tag values for %s differ: tempo %v, gateway %v", test.Tag, tempoValues, gwValues)
		}
	}
}

func httpJSON(t *testing.T, url string) map[string][]string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, data)
	}
	data, _ := io.ReadAll(resp.Body)
	return decodeJSON(t, data)
}

func decodeJSON(t *testing.T, data []byte) map[string][]string {
	t.Helper()
	var body struct {
		Scopes []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode scopes: %v (%s)", err, data)
	}
	out := make(map[string][]string, len(body.Scopes))
	for _, scope := range body.Scopes {
		out[scope.Name] = sorted(scope.Tags)
	}
	return out
}

func equalScopeMaps(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, values := range a {
		if !equal(values, b[name]) {
			return false
		}
	}
	return true
}

func tagValues(t *testing.T, url string) []string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, data)
	}
	data, _ := io.ReadAll(resp.Body)
	return decodeTagValues(t, data)
}

func decodeTagValues(t *testing.T, data []byte) []string {
	t.Helper()
	var body struct {
		TagValues []struct {
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode tag values: %v (%s)", err, data)
	}
	out := make([]string, 0, len(body.TagValues))
	for _, value := range body.TagValues {
		out = append(out, value.Value)
	}
	sort.Strings(out)
	return out
}

func runTraceComparisons(t *testing.T, c *corpus.Corpus, tempoURL string, gateway http.Handler) {
	t.Helper()
	for _, test := range c.TraceTests {
		t.Run("trace/"+test.TraceID, func(t *testing.T) {
			tempoSpans := fetchTempoTrace(t, tempoURL, test.TraceID)
			gatewaySpans := fetchGatewayTrace(t, gateway, test.TraceID)
			if !equalNormalizedSpans(tempoSpans, gatewaySpans) {
				t.Fatalf("trace %s differs:\ntempo:   %s\ngateway: %s", test.TraceID, formatSpans(tempoSpans), formatSpans(gatewaySpans))
			}
			if len(tempoSpans) != test.SpanCount {
				t.Fatalf("corpus/reference mismatch: tempo span count = %d, want %d", len(tempoSpans), test.SpanCount)
			}
			for _, want := range test.SpanIDs {
				found := false
				for _, span := range tempoSpans {
					if span.ID == want {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("corpus/reference mismatch: tempo missing span %s", want)
				}
			}
		})
	}
}

type normalizedSpan struct {
	ID           string
	ParentID     string
	Name         string
	StartNanos   int64
	DurationNano int64
	Status       string
	Kind         string
	EventNames   []string
	LinkIDs      []string
	Attributes   map[string]string
	Service      string
}

func fetchTempoTrace(t *testing.T, baseURL, traceID string) []normalizedSpan {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v2/traces/"+traceID, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET tempo trace: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tempo trace = %d: %s", resp.StatusCode, data)
	}
	var body struct {
		Trace struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []otlpJSONValue `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []tempoJSONSpan `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode tempo trace: %v (%s)", err, data)
	}
	out := make([]normalizedSpan, 0)
	for _, resource := range body.Trace.ResourceSpans {
		attrs := attrMap(resource.Resource.Attributes)
		service := attrs["service.name"]
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				spanAttrs := attrMap(span.Attributes)
				for key, value := range attrs {
					if _, exists := spanAttrs[key]; !exists {
						spanAttrs[key] = value
					}
				}
				flat := normalizedSpan{
					ID:         decodeTempoID(span.SpanID),
					ParentID:   decodeTempoID(span.ParentSpanID),
					Name:       span.Name,
					Status:     normalizeTempoStatus(span.Status.Code),
					Kind:       normalizeTempoKind(span.Kind),
					Attributes: spanAttrs,
					Service:    service,
				}
				flat.StartNanos, _ = strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
				end, _ := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
				flat.DurationNano = end - flat.StartNanos
				for _, event := range span.Events {
					flat.EventNames = append(flat.EventNames, event.Name)
				}
				for _, link := range span.Links {
					flat.LinkIDs = append(flat.LinkIDs, decodeTempoID(link.TraceID))
				}
				out = append(out, flat)
			}
		}
	}
	sortNormalized(out)
	return out
}

type otlpJSONValue struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"`
		BoolValue   *bool    `json:"boolValue"`
		DoubleValue *float64 `json:"doubleValue"`
	} `json:"value"`
}

type tempoJSONSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId"`
	Name              string          `json:"name"`
	Kind              string          `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []otlpJSONValue `json:"attributes"`
	Status            struct {
		Code string `json:"code"`
	} `json:"status"`
	Events []struct {
		Name string `json:"name"`
	} `json:"events"`
	Links []struct {
		TraceID string `json:"traceId"`
		SpanID  string `json:"spanId"`
	} `json:"links"`
}

func attrMap(attrs []otlpJSONValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		switch {
		case attr.Value.StringValue != nil:
			out[attr.Key] = *attr.Value.StringValue
		case attr.Value.IntValue != nil:
			out[attr.Key] = *attr.Value.IntValue
		case attr.Value.BoolValue != nil:
			out[attr.Key] = strconv.FormatBool(*attr.Value.BoolValue)
		case attr.Value.DoubleValue != nil:
			out[attr.Key] = strconv.FormatFloat(*attr.Value.DoubleValue, 'g', -1, 64)
		}
	}
	return out
}

func fetchGatewayTrace(t *testing.T, gateway http.Handler, traceID string) []normalizedSpan {
	t.Helper()
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/traces/"+traceID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway trace = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Trace struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []otlpJSONValue `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []struct {
						TraceID           string          `json:"traceId"`
						SpanID            string          `json:"spanId"`
						ParentSpanID      string          `json:"parentSpanId"`
						Name              string          `json:"name"`
						Kind              int             `json:"kind"`
						StartTimeUnixNano string          `json:"startTimeUnixNano"`
						EndTimeUnixNano   string          `json:"endTimeUnixNano"`
						Attributes        []otlpJSONValue `json:"attributes"`
						Status            struct {
							Code int `json:"code"`
						} `json:"status"`
						Events []struct {
							Name string `json:"name"`
						} `json:"events"`
						Links []struct {
							TraceID string `json:"traceId"`
						} `json:"links"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode gateway trace: %v", err)
	}
	out := make([]normalizedSpan, 0)
	for _, resource := range body.Trace.ResourceSpans {
		attrs := attrMap(resource.Resource.Attributes)
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				spanAttrs := attrMap(span.Attributes)
				for key, value := range attrs {
					if _, exists := spanAttrs[key]; !exists {
						spanAttrs[key] = value
					}
				}
				flat := normalizedSpan{
					ID:         strings.ToLower(span.SpanID),
					ParentID:   strings.ToLower(span.ParentSpanID),
					Name:       span.Name,
					Status:     gatewayStatus(span.Status.Code),
					Kind:       gatewayKind(span.Kind),
					Attributes: spanAttrs,
					Service:    attrs["service.name"],
				}
				flat.StartNanos, _ = strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
				end, _ := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
				flat.DurationNano = end - flat.StartNanos
				for _, event := range span.Events {
					flat.EventNames = append(flat.EventNames, event.Name)
				}
				for _, link := range span.Links {
					flat.LinkIDs = append(flat.LinkIDs, strings.ToLower(link.TraceID))
				}
				out = append(out, flat)
			}
		}
	}
	sortNormalized(out)
	return out
}

func decodeTempoID(id string) string {
	if id == "" {
		return ""
	}
	data, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		// Tempo also emits hex in some configurations; accept it.
		return strings.ToLower(id)
	}
	if len(data) == 0 {
		return ""
	}
	return hex.EncodeToString(data)
}

func normalizeTempoStatus(code string) string {
	code = strings.ToLower(code)
	code = strings.TrimPrefix(code, "status_code_")
	if code == "" {
		return "unset"
	}
	return code
}

func normalizeTempoKind(kind string) string {
	kind = strings.ToLower(kind)
	kind = strings.TrimPrefix(kind, "span_kind_")
	if kind == "" {
		return "unspecified"
	}
	return kind
}

func gatewayStatus(code int) string {
	switch code {
	case 1:
		return "ok"
	case 2:
		return "error"
	default:
		return "unset"
	}
}

func gatewayKind(kind int) string {
	switch kind {
	case 1:
		return "internal"
	case 2:
		return "server"
	case 3:
		return "client"
	case 4:
		return "producer"
	case 5:
		return "consumer"
	default:
		return "unspecified"
	}
}

func sortNormalized(spans []normalizedSpan) {
	for i := range spans {
		sort.Strings(spans[i].EventNames)
		sort.Strings(spans[i].LinkIDs)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].ID < spans[j].ID })
}

func equalNormalizedSpans(a, b []normalizedSpan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].ParentID != b[i].ParentID || a[i].Name != b[i].Name ||
			a[i].StartNanos != b[i].StartNanos || a[i].DurationNano != b[i].DurationNano ||
			a[i].Status != b[i].Status || a[i].Kind != b[i].Kind {
			return false
		}
		if !equal(a[i].EventNames, b[i].EventNames) || !equal(a[i].LinkIDs, b[i].LinkIDs) {
			return false
		}
		if !equalAttrMaps(a[i].Attributes, b[i].Attributes) {
			return false
		}
	}
	return true
}

func equalAttrMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func formatSpans(spans []normalizedSpan) string {
	data, _ := json.Marshal(spans)
	return string(data)
}

func newGateway(t *testing.T, c *corpus.Corpus, base time.Time) http.Handler {
	t.Helper()
	spans := corpusSpans(c, base)
	api := tempo.NewAPI(corpusFetcher{spans: spans}, "compat")
	api.Now = func() time.Time { return base.Add(5 * time.Minute) }
	mux := http.NewServeMux()
	api.Register(mux)
	return mux
}

type corpusFetcher struct {
	spans []tempo.Span
}

func (f corpusFetcher) GetTrace(_ context.Context, traceID string, _ *time.Time, _ *time.Time) ([]tempo.Span, error) {
	out := make([]tempo.Span, 0)
	for _, span := range f.spans {
		if span.TraceID == traceID {
			out = append(out, span)
		}
	}
	return out, nil
}

func (f corpusFetcher) FetchRange(context.Context, time.Time, time.Time) ([]tempo.Span, error) {
	return f.spans, nil
}

func corpusSpans(c *corpus.Corpus, base time.Time) []tempo.Span {
	spans := make([]tempo.Span, 0)
	for _, trace := range c.Traces {
		for _, source := range trace.Spans {
			span := tempo.Span{
				Timestamp:          corpus.SpanStart(base, source),
				TraceID:            trace.TraceID,
				SpanID:             source.SpanID,
				ParentSpanID:       source.ParentSpanID,
				SpanName:           source.Name,
				SpanKind:           source.Kind,
				ServiceName:        source.Service,
				ResourceAttributes: corpus.StringAttributes(source.ResourceAttributes),
				ScopeName:          source.ScopeName,
				ScopeVersion:       source.ScopeVersion,
				SpanAttributes:     corpus.StringAttributes(source.Attributes),
				Duration:           uint64(source.DurationNanos),
				StatusCode:         source.Status,
				StatusMessage:      source.StatusMessage,
			}
			if span.ScopeName == "" {
				span.ScopeName = "corpus"
			}
			if span.ScopeVersion == "" {
				span.ScopeVersion = "1.0"
			}
			for _, event := range source.Events {
				span.Events = append(span.Events, tempo.Event{
					Timestamp:  span.Timestamp.Add(time.Duration(event.OffsetNanos)),
					Name:       event.Name,
					Attributes: corpus.StringAttributes(event.Attributes),
				})
			}
			for _, link := range source.Links {
				span.Links = append(span.Links, tempo.Link{
					TraceID:    link.TraceID,
					SpanID:     link.SpanID,
					Attributes: corpus.StringAttributes(link.Attributes),
				})
			}
			spans = append(spans, span)
		}
	}
	return spans
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
