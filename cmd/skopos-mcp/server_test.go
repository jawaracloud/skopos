package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runServer(t *testing.T, server *Server, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := server.Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var responses []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		responses = append(responses, response)
	}
	return responses
}

func newTestMCP(t *testing.T, gatewayURL string) *Server {
	t.Helper()
	server, err := NewServer(Config{GatewayURL: gatewayURL, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestInitializeAndToolsList(t *testing.T) {
	gateway := httptest.NewServer(http.NotFoundHandler())
	defer gateway.Close()
	responses := runServer(t, newTestMCP(t, gateway.URL),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	result := responses[0]["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol = %v", result["protocolVersion"])
	}
	serverInfo := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "skopos" || serverInfo["version"] != "test" {
		t.Fatalf("serverInfo = %v", serverInfo)
	}
	toolsResult := responses[1]["result"].(map[string]any)
	toolList := toolsResult["tools"].([]any)
	if len(toolList) != 12 {
		t.Fatalf("tools = %d", len(toolList))
	}
}

func TestContextToolAndAuth(t *testing.T) {
	var sawAuth bool
	var sawPath, sawQuery string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawQuery = r.URL.Path, r.URL.RawQuery
		user, pass, ok := r.BasicAuth()
		sawAuth = ok && user == "viewer" && pass == "secret"
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema":"ctx/v1","name":"demo","known":true,"role":"latency"}`))
	}))
	defer gateway.Close()
	server, err := NewServer(Config{
		GatewayURL: gateway.URL, Username: "viewer", Password: "secret", Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	responses := runServer(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_metric_context","arguments":{"name":"demo","detail":"minimal"}}}`,
	)
	if len(responses) != 1 {
		t.Fatalf("responses = %d", len(responses))
	}
	if !sawAuth {
		t.Fatal("gateway did not receive Basic auth")
	}
	if sawPath != "/api/v1/context/metrics/demo" || !strings.Contains(sawQuery, "detail=minimal") {
		t.Fatalf("path/query = %q/%q", sawPath, sawQuery)
	}
	result := responses[0]["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("tool error = %v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), `"known":true`) {
		t.Fatalf("content = %v", content)
	}
}

func TestQueryToolsRouteToGateway(t *testing.T) {
	var paths []string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer gateway.Close()
	server := newTestMCP(t, gateway.URL)
	runServer(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_metrics","arguments":{"service":"checkout","role":"latency","limit":5}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query_promql","arguments":{"query":"up","time":"1793491500"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"query_logql","arguments":{"query":"{service_name=\"checkout\"}","start":"1","end":"2","limit":10}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_traces","arguments":{"query":"{ }","start":"1","end":"2"}}}`,
	)
	joined := strings.Join(paths, "\n")
	for _, want := range []string{
		"/api/v1/context/metrics?",
		"service=checkout",
		"role=latency",
		"limit=5",
		"/api/v1/query?",
		"query=up",
		"/loki/api/v1/query_range?",
		"/api/search?",
		"q=%7B+%7D",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("paths %q missing %q", joined, want)
		}
	}
}

func TestToolErrors(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer gateway.Close()
	server := newTestMCP(t, gateway.URL)
	responses := runServer(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_metric_context","arguments":{"name":"demo"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_metric_context","arguments":{"unknown":true}}}`,
	)
	for _, response := range responses {
		result := response["result"].(map[string]any)
		if result["isError"] != true {
			t.Fatalf("expected tool error: %v", response)
		}
	}
}

func TestMalformedAndUnknownRequests(t *testing.T) {
	gateway := httptest.NewServer(http.NotFoundHandler())
	defer gateway.Close()
	responses := runServer(t, newTestMCP(t, gateway.URL),
		`not json`,
		`{"jsonrpc":"2.0","id":2,"method":"missing"}`,
	)
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	if responses[0]["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("parse error = %v", responses[0])
	}
	if responses[1]["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("method error = %v", responses[1])
	}
}

func TestResponseSizeLimit(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer gateway.Close()
	server, err := NewServer(Config{GatewayURL: gateway.URL, MaxBytes: 10, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	responses := runServer(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_context_coverage","arguments":{}}}`,
	)
	result := responses[0]["result"].(map[string]any)
	if result["isError"] != true || !strings.Contains(result["content"].([]any)[0].(map[string]any)["text"].(string), "exceeds") {
		t.Fatalf("size error = %v", result)
	}
}

func TestInitializeProtocolFallback(t *testing.T) {
	gateway := httptest.NewServer(http.NotFoundHandler())
	defer gateway.Close()
	responses := runServer(t, newTestMCP(t, gateway.URL),
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
	)
	result := responses[0]["result"].(map[string]any)
	if result["protocolVersion"] != protocolVersion {
		t.Fatalf("protocol = %v", result["protocolVersion"])
	}
}

func TestHTTPTransport(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema":"ctx/v1","known":true}`))
	}))
	defer gateway.Close()
	handler := newTestMCP(t, gateway.URL).HTTPHandler()

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize = %d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize body = %s", rec.Body)
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_context_coverage","arguments":{}}}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content"`) {
		t.Fatalf("tool = %d %s", rec.Code, rec.Body)
	}
	if got := getHTTP(t, handler, http.MethodGet, "/healthz"); got.Code != http.StatusOK {
		t.Fatalf("health = %d", got.Code)
	}
	if got := getHTTP(t, handler, http.MethodGet, "/mcp"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get mcp = %d", got.Code)
	}
	if got := getHTTP(t, handler, http.MethodDelete, "/mcp"); got.Code != http.StatusNoContent {
		t.Fatalf("delete mcp = %d", got.Code)
	}
}

func TestHTTPAuth(t *testing.T) {
	server, err := NewServer(Config{
		GatewayURL: "http://127.0.0.1:1", HTTPUsername: "viewer", HTTPPassword: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.HTTPHandler()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
	req.SetBasicAuth("viewer", "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated = %d body=%s", rec.Code, rec.Body)
	}
	if got := getHTTP(t, handler, http.MethodGet, "/healthz"); got.Code != http.StatusOK {
		t.Fatalf("health auth bypass = %d", got.Code)
	}
}

func TestHTTPSSE(t *testing.T) {
	gateway := httptest.NewServer(http.NotFoundHandler())
	defer gateway.Close()
	handler := newTestMCP(t, gateway.URL).HTTPHandler()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sse = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(rec.Body.String(), "event: message\ndata: ") {
		t.Fatalf("sse body = %q", rec.Body.String())
	}
}

func TestHTTPRequestTooLarge(t *testing.T) {
	server, err := NewServer(Config{GatewayURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	rec := httptest.NewRecorder()
	server.HTTPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
}

func getHTTP(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("SKOPOS_GATEWAY_URL", "http://gateway:9000")
	t.Setenv("SKOPOS_AUTH_USERNAME", "viewer")
	t.Setenv("SKOPOS_AUTH_PASSWORD", "secret")
	t.Setenv("SKOPOS_MCP_MAX_BYTES", "1234")
	t.Setenv("SKOPOS_MCP_HTTP_ADDR", "127.0.0.1:8090")
	t.Setenv("SKOPOS_MCP_HTTP_USERNAME", "mcp")
	t.Setenv("SKOPOS_MCP_HTTP_PASSWORD", "pass")
	cfg := ConfigFromEnv()
	if cfg.GatewayURL != "http://gateway:9000" || cfg.Username != "viewer" || cfg.Password != "secret" ||
		cfg.MaxBytes != 1234 || cfg.HTTPAddr != "127.0.0.1:8090" ||
		cfg.HTTPUsername != "mcp" || cfg.HTTPPassword != "pass" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestToolSchemasAreValidObjects(t *testing.T) {
	for _, tool := range tools() {
		if tool.Name == "" || tool.Description == "" {
			t.Fatalf("incomplete tool = %#v", tool)
		}
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("%s schema = %#v", tool.Name, tool.InputSchema)
		}
		if _, ok := tool.InputSchema["required"]; ok {
			// required is optional for tools with no required fields
			continue
		}
	}
}
