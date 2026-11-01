// Command skopos-mcp is a small stdio MCP server for AI SRE agents. It exposes
// the compact Skopos context API and the existing query APIs as MCP tools.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jawaracloud/skopos/internal/auth"
)

const (
	defaultGatewayURL = "http://127.0.0.1:8080"
	defaultMaxBytes   = 64 << 10
	protocolVersion   = "2025-06-18"
	sessionTTL        = 30 * time.Minute
)

var supportedProtocolVersions = map[string]struct{}{
	"2024-11-05": {},
	"2025-03-26": {},
	"2025-06-18": {},
}

// Config configures the MCP server.
type Config struct {
	GatewayURL   string
	Username     string
	Password     string
	MaxBytes     int64
	Timeout      time.Duration
	Version      string
	HTTPClient   *http.Client
	HTTPAddr     string
	HTTPUsername string
	HTTPPassword string
}

// Server is a stdio MCP server.
type Server struct {
	gateway  *Gateway
	version  string
	timeout  time.Duration
	httpAuth *auth.Basic

	sessionsMu sync.Mutex
	sessions   map[string]*mcpSession
}

type mcpSession struct {
	lastSeen time.Time
	done     chan struct{}
}

// NewServer validates cfg and constructs a server.
func NewServer(cfg Config) (*Server, error) {
	u, err := url.Parse(cfg.GatewayURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid gateway URL %q", cfg.GatewayURL)
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	var httpAuth *auth.Basic
	if cfg.HTTPUsername != "" || cfg.HTTPPassword != "" {
		if cfg.HTTPUsername == "" || cfg.HTTPPassword == "" {
			return nil, errors.New("both MCP HTTP username and password are required")
		}
		var err error
		httpAuth, err = auth.New(cfg.HTTPUsername, cfg.HTTPPassword)
		if err != nil {
			return nil, err
		}
	}
	return &Server{
		gateway: &Gateway{
			base: u, username: cfg.Username, password: cfg.Password,
			maxBytes: cfg.MaxBytes, client: client,
		},
		version:  cfg.Version,
		timeout:  cfg.Timeout,
		httpAuth: httpAuth,
		sessions: make(map[string]*mcpSession),
	}, nil
}

// Serve reads newline-delimited JSON-RPC messages and writes responses.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	writer := bufio.NewWriter(out)
	defer writer.Flush()
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		response := s.handleLine(line)
		if response == nil {
			continue
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		if _, err := writer.Write(encoded); err != nil {
			return err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleLine(line []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}}
	}
	notification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.JSONRPC != "2.0" || req.Method == "" {
		if notification {
			return nil
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32600, Message: "invalid request"}}
	}
	response := s.handleRequest(&req)
	if notification {
		return nil
	}
	return response
}

func (s *Server) handleRequest(req *rpcRequest) *rpcResponse {
	response := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				response.Error = &rpcError{Code: -32602, Message: "invalid initialize params"}
				return response
			}
		}
		version := protocolVersion
		if _, ok := supportedProtocolVersions[params.ProtocolVersion]; ok {
			version = params.ProtocolVersion
		}
		response.Result = map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]string{"name": "skopos", "version": s.version},
			"instructions": "Call get_service_context or list_metrics before querying. " +
				"Use get_metric_context detail=minimal by default and request labels/caveats only when needed. " +
				"Do not infer missing context; known:false means the catalog has no entry.",
		}
	case "notifications/initialized", "notifications/cancelled":
		return nil
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": tools()}
	case "tools/call":
		result, err := s.callTool(req.Params)
		if err != nil {
			response.Result = toolError(err)
		} else {
			response.Result = result
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return response
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func toolText(text string) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}}
}

func toolError(err error) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: err.Error()}}, IsError: true}
}

func (s *Server) callTool(raw json.RawMessage) (toolResult, error) {
	var params toolCallParams
	if err := json.Unmarshal(raw, &params); err != nil || params.Name == "" {
		return toolResult{}, errors.New("tools/call requires name and arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	switch params.Name {
	case "list_metrics":
		var args struct {
			Service string `json:"service"`
			Role    string `json:"role"`
			Limit   int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		values := url.Values{}
		add(values, "service", args.Service)
		add(values, "role", args.Role)
		addLimit(values, args.Limit)
		return s.getContext(ctx, "/api/v1/context/metrics", values)
	case "get_metric_context":
		var args struct {
			Name    string `json:"name"`
			Detail  string `json:"detail"`
			Include string `json:"include"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if args.Name == "" {
			return toolResult{}, errors.New("name is required")
		}
		values := url.Values{}
		add(values, "detail", args.Detail)
		add(values, "include", args.Include)
		return s.getContext(ctx, "/api/v1/context/metrics/"+url.PathEscape(args.Name), values)
	case "get_service_context":
		var args struct {
			Service string `json:"service"`
			Limit   int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if args.Service == "" {
			return toolResult{}, errors.New("service is required")
		}
		values := url.Values{}
		addLimit(values, args.Limit)
		return s.getContext(ctx, "/api/v1/context/services/"+url.PathEscape(args.Service), values)
	case "list_logs":
		var args struct {
			Service string `json:"service"`
			Limit   int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		values := url.Values{}
		add(values, "service", args.Service)
		addLimit(values, args.Limit)
		return s.getContext(ctx, "/api/v1/context/logs", values)
	case "get_log_context":
		var args struct {
			ID      string `json:"id"`
			Detail  string `json:"detail"`
			Include string `json:"include"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if args.ID == "" {
			return toolResult{}, errors.New("id is required")
		}
		values := url.Values{}
		add(values, "detail", args.Detail)
		add(values, "include", args.Include)
		return s.getContext(ctx, "/api/v1/context/logs/"+url.PathEscape(args.ID), values)
	case "list_traces":
		var args struct {
			Service string `json:"service"`
			Limit   int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		values := url.Values{}
		add(values, "service", args.Service)
		addLimit(values, args.Limit)
		return s.getContext(ctx, "/api/v1/context/traces", values)
	case "get_trace_context":
		var args struct {
			ID      string `json:"id"`
			Detail  string `json:"detail"`
			Include string `json:"include"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if args.ID == "" {
			return toolResult{}, errors.New("id is required")
		}
		values := url.Values{}
		add(values, "detail", args.Detail)
		add(values, "include", args.Include)
		return s.getContext(ctx, "/api/v1/context/traces/"+url.PathEscape(args.ID), values)
	case "get_context_coverage":
		var args struct {
			Observed bool    `json:"observed"`
			Min      float64 `json:"min"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		values := url.Values{}
		if args.Observed {
			values.Set("observed", "true")
		}
		if args.Min != 0 {
			values.Set("min", strconv.FormatFloat(args.Min, 'f', -1, 64))
		}
		return s.getContext(ctx, "/api/v1/context/coverage", values)
	case "get_context_suggestions":
		var args struct {
			Limit int `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		values := url.Values{}
		addLimit(values, args.Limit)
		return s.getContext(ctx, "/api/v1/context/suggestions", values)
	case "get_well_known":
		return s.getContext(ctx, "/.well-known/skopos.json", nil)
	case "query_promql":
		var args struct {
			Query string `json:"query"`
			Time  string `json:"time"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if args.Query == "" {
			return toolResult{}, errors.New("query is required")
		}
		values := url.Values{"query": {args.Query}}
		add(values, "time", args.Time)
		return s.getContext(ctx, "/api/v1/query", values)
	case "query_logql":
		var args struct {
			Query     string `json:"query"`
			Start     string `json:"start"`
			End       string `json:"end"`
			Direction string `json:"direction"`
			Limit     int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if args.Query == "" {
			return toolResult{}, errors.New("query is required")
		}
		values := url.Values{"query": {args.Query}}
		add(values, "start", args.Start)
		add(values, "end", args.End)
		add(values, "direction", args.Direction)
		addLimit(values, args.Limit)
		return s.getContext(ctx, "/loki/api/v1/query_range", values)
	case "search_traces":
		var args struct {
			Query string `json:"query"`
			Start string `json:"start"`
			End   string `json:"end"`
			Limit int    `json:"limit"`
		}
		if err := decodeArgs(params.Arguments, &args); err != nil {
			return toolResult{}, err
		}
		if args.Query == "" {
			return toolResult{}, errors.New("query is required")
		}
		values := url.Values{"q": {args.Query}}
		add(values, "start", args.Start)
		add(values, "end", args.End)
		addLimit(values, args.Limit)
		return s.getContext(ctx, "/api/search", values)
	default:
		return toolResult{}, fmt.Errorf("unknown tool %q", params.Name)
	}
}

func (s *Server) getContext(ctx context.Context, path string, values url.Values) (toolResult, error) {
	body, err := s.gateway.get(ctx, path, values)
	if err != nil {
		return toolResult{}, err
	}
	return toolText(string(body)), nil
}

func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func add(values url.Values, key, value string) {
	if value != "" {
		values.Set(key, value)
	}
}

func addLimit(values url.Values, limit int) {
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
}

// Gateway is the HTTP client used by MCP tools.
type Gateway struct {
	base     *url.URL
	username string
	password string
	maxBytes int64
	client   *http.Client
}

func (g *Gateway) get(ctx context.Context, path string, values url.Values) ([]byte, error) {
	u := *g.base
	u.Path = strings.TrimRight(g.base.Path, "/") + path
	u.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if g.username != "" || g.password != "" {
		req.SetBasicAuth(g.username, g.password)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, g.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read gateway response: %w", err)
	}
	if int64(len(body)) > g.maxBytes {
		return nil, fmt.Errorf("gateway response exceeds %d bytes; narrow the query", g.maxBytes)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("gateway returned %s: %s", resp.Status, truncate(string(body), 300))
	}
	return body, nil
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

// HTTPHandler returns the remote Streamable-HTTP-style MCP endpoint. It
// accepts one JSON-RPC request per POST /mcp; JSON is the default response and
// text/event-stream is returned when the client asks for it.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("POST /mcp", s.handleHTTPMCP)
	mux.HandleFunc("GET /mcp", s.handleHTTPStream)
	mux.HandleFunc("DELETE /mcp", s.handleHTTPDelete)
	var handler http.Handler = mux
	if s.httpAuth != nil {
		// Health checks stay open for probes; MCP itself is protected.
		handler = s.httpAuth.Wrap(handler, "/healthz")
	}
	return handler
}

func (s *Server) handleHTTPMCP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request", http.StatusBadRequest)
		return
	}
	var request rpcRequest
	_ = json.Unmarshal(body, &request)
	if request.Method == "initialize" {
		sessionID := newSessionID()
		if sessionID == "" {
			http.Error(w, "cannot create session", http.StatusInternalServerError)
			return
		}
		s.addSession(sessionID)
		w.Header().Set("Mcp-Session-Id", sessionID)
	} else if !s.validSession(r.Header.Get("Mcp-Session-Id")) {
		http.Error(w, "valid Mcp-Session-Id is required", http.StatusBadRequest)
		return
	}
	response := s.handleLine(body)
	if response == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "cannot encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", encoded)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
	_, _ = w.Write([]byte("\n"))
}

func (s *Server) addSession(id string) {
	s.sessionsMu.Lock()
	s.pruneSessionsLocked()
	s.sessions[id] = &mcpSession{lastSeen: time.Now(), done: make(chan struct{})}
	s.sessionsMu.Unlock()
}

func (s *Server) validSession(id string) bool {
	if id == "" {
		return false
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.pruneSessionsLocked()
	session, ok := s.sessions[id]
	if ok {
		session.lastSeen = time.Now()
	}
	return ok
}

func (s *Server) session(id string) *mcpSession {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	return s.sessions[id]
}

func (s *Server) deleteSession(id string) {
	s.sessionsMu.Lock()
	if session, ok := s.sessions[id]; ok {
		close(session.done)
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()
}

func (s *Server) pruneSessionsLocked() {
	cutoff := time.Now().Add(-sessionTTL)
	for id, session := range s.sessions {
		if session.lastSeen.Before(cutoff) {
			close(session.done)
			delete(s.sessions, id)
		}
	}
}

func (s *Server) handleHTTPStream(w http.ResponseWriter, r *http.Request) {
	session := s.session(r.Header.Get("Mcp-Session-Id"))
	if session == nil {
		http.Error(w, "valid Mcp-Session-Id is required", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-session.done:
			return
		case <-ticker.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleHTTPDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteSession(r.Header.Get("Mcp-Session-Id"))
	w.WriteHeader(http.StatusNoContent)
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// ConfigFromEnv reads the MCP server configuration.
func ConfigFromEnv() Config {
	maxBytes := int64(defaultMaxBytes)
	if value := os.Getenv("SKOPOS_MCP_MAX_BYTES"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			maxBytes = parsed
		}
	}
	return Config{
		GatewayURL:   envOr("SKOPOS_GATEWAY_URL", defaultGatewayURL),
		Username:     os.Getenv("SKOPOS_AUTH_USERNAME"),
		Password:     os.Getenv("SKOPOS_AUTH_PASSWORD"),
		MaxBytes:     maxBytes,
		HTTPAddr:     os.Getenv("SKOPOS_MCP_HTTP_ADDR"),
		HTTPUsername: os.Getenv("SKOPOS_MCP_HTTP_USERNAME"),
		HTTPPassword: os.Getenv("SKOPOS_MCP_HTTP_PASSWORD"),
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
