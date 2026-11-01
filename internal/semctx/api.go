package semctx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultListLimit    = 20
	maxListLimit        = 50
	defaultServiceLimit = 10
	maxServiceLimit     = 20
)

type detailLevel string

const (
	detailMinimal  detailLevel = "minimal"
	detailStandard detailLevel = "standard"
	detailFull     detailLevel = "full"
)

var allowedIncludes = map[string]struct{}{
	"labels": {}, "query": {}, "runbook": {}, "related": {},
	"caveats": {}, "provenance": {},
}

var allowedRoles = map[string]struct{}{
	"latency": {}, "traffic": {}, "error_rate": {}, "saturation": {},
	"resource": {}, "business_kpi": {}, "build_info": {},
}

// API serves the compact context endpoints. It is intentionally separate from
// the Prometheus/Loki/Tempo query handlers.
type API struct {
	Catalog *Catalog
}

// NewAPI returns a context API. A nil catalog becomes an empty catalog.
func NewAPI(catalog *Catalog) *API {
	if catalog == nil {
		catalog = EmptyCatalog()
	}
	return &API{Catalog: catalog}
}

// Register attaches the context routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/context", a.handleIndex)
	mux.HandleFunc("GET /api/v1/context/{$}", a.handleIndex)
	mux.HandleFunc("GET /api/v1/context/metrics", a.handleList)
	mux.HandleFunc("GET /api/v1/context/metrics/{name}", a.handleMetric)
	mux.HandleFunc("GET /api/v1/context/services/{service}", a.handleService)
}

type metricResponse struct {
	Schema      string            `json:"schema"`
	Name        string            `json:"name"`
	Known       bool              `json:"known"`
	Type        string            `json:"type,omitempty"`
	Unit        string            `json:"unit,omitempty"`
	Role        string            `json:"role,omitempty"`
	Direction   string            `json:"direction,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Service     string            `json:"service,omitempty"`
	Description string            `json:"description,omitempty"`
	Flags       []string          `json:"flags,omitempty"`
	Sources     []string          `json:"sources,omitempty"`
	Query       string            `json:"query,omitempty"`
	Runbook     string            `json:"runbook,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Related     []string          `json:"related,omitempty"`
	Caveats     []string          `json:"caveats,omitempty"`
	Provenance  []provenance      `json:"provenance,omitempty"`
}

type provenance struct {
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
}

type listItem struct {
	Name    string   `json:"name"`
	Type    string   `json:"type,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Role    string   `json:"role,omitempty"`
	Service string   `json:"service,omitempty"`
	Flags   []string `json:"flags,omitempty"`
}

type listResponse struct {
	Schema  string     `json:"schema"`
	Version string     `json:"version"`
	Items   []listItem `json:"items"`
	Total   int        `json:"total"`
	HasMore bool       `json:"has_more,omitempty"`
}

type serviceResponse struct {
	Schema      string     `json:"schema"`
	Version     string     `json:"version"`
	Service     string     `json:"service"`
	Known       bool       `json:"known"`
	Description string     `json:"description,omitempty"`
	Owner       string     `json:"owner,omitempty"`
	Runbook     string     `json:"runbook,omitempty"`
	Metrics     []listItem `json:"metrics,omitempty"`
	Logs        []string   `json:"logs,omitempty"`
	Traces      []string   `json:"traces,omitempty"`
	Flags       []string   `json:"flags,omitempty"`
	HasMore     bool       `json:"has_more,omitempty"`
}

func (a *API) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, a.Catalog.Version, map[string]any{
		"schema":        SchemaVersion,
		"version":       a.Catalog.Version,
		"api":           "/api/v1/context",
		"detail_levels": []string{string(detailMinimal), string(detailStandard), string(detailFull)},
		"include":       []string{"labels", "query", "runbook", "related", "caveats", "provenance"},
		"signals":       []string{"metrics", "services"},
	})
}

func (a *API) handleMetric(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	detail, err := parseDetail(r.URL.Query().Get("detail"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	include, err := parseInclude(r.URL.Query().Get("include"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	metric, known := a.Catalog.Metrics[name]
	if !known {
		writeJSON(w, r, a.Catalog.Version, metricResponse{
			Schema: SchemaVersion,
			Name:   name,
			Known:  false,
			Flags:  []string{"missing_context"},
		})
		return
	}
	out := metricResponse{
		Schema:      SchemaVersion,
		Name:        name,
		Known:       true,
		Type:        metric.Type,
		Unit:        metric.Unit,
		Role:        metric.Role,
		Direction:   metric.Direction,
		Owner:       metric.Owner,
		Service:     metric.Service,
		Description: metric.Description,
		Flags:       append([]string(nil), metric.Flags...),
		Sources:     []string{"catalog"},
	}
	if detail == detailStandard || detail == detailFull {
		out.Query = metric.Query
		out.Runbook = metric.Runbook
		out.Labels = cloneLabels(metric.Labels)
	}
	if detail == detailFull {
		out.Related = append([]string(nil), metric.Related...)
		out.Caveats = append([]string(nil), metric.Caveats...)
		out.Provenance = []provenance{{Source: "catalog", Confidence: 1}}
	}
	if _, ok := include["query"]; ok {
		out.Query = metric.Query
	}
	if _, ok := include["runbook"]; ok {
		out.Runbook = metric.Runbook
	}
	if _, ok := include["labels"]; ok {
		out.Labels = cloneLabels(metric.Labels)
	}
	if _, ok := include["related"]; ok {
		out.Related = append([]string(nil), metric.Related...)
	}
	if _, ok := include["caveats"]; ok {
		out.Caveats = append([]string(nil), metric.Caveats...)
	}
	if _, ok := include["provenance"]; ok {
		out.Provenance = []provenance{{Source: "catalog", Confidence: 1}}
	}
	writeJSON(w, r, a.Catalog.Version, out)
}

func (a *API) handleList(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	role := r.URL.Query().Get("role")
	if role != "" {
		if _, ok := allowedRoles[role]; !ok {
			writeError(w, http.StatusBadRequest, "invalid role")
			return
		}
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"), defaultListLimit, maxListLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	names := make([]string, 0, len(a.Catalog.Metrics))
	for name, metric := range a.Catalog.Metrics {
		if service != "" && metric.Service != service {
			continue
		}
		if role != "" && metric.Role != role {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	total := len(names)
	if len(names) > limit {
		names = names[:limit]
	}
	items := make([]listItem, 0, len(names))
	for _, name := range names {
		metric := a.Catalog.Metrics[name]
		items = append(items, listItem{
			Name: name, Type: metric.Type, Unit: metric.Unit,
			Role: metric.Role, Service: metric.Service,
			Flags: append([]string(nil), metric.Flags...),
		})
	}
	writeJSON(w, r, a.Catalog.Version, listResponse{
		Schema: SchemaVersion, Version: a.Catalog.Version,
		Items: items, Total: total, HasMore: total > len(items),
	})
}

func (a *API) handleService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("service")
	limit, err := parseLimit(r.URL.Query().Get("limit"), defaultServiceLimit, maxServiceLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	service, known := a.Catalog.Services[name]
	if !known {
		writeJSON(w, r, a.Catalog.Version, serviceResponse{
			Schema: SchemaVersion, Version: a.Catalog.Version,
			Service: name, Known: false, Flags: []string{"missing_context"},
		})
		return
	}
	total := len(service.Metrics)
	metricNames := service.Metrics
	if total > limit {
		metricNames = metricNames[:limit]
	}
	metrics := make([]listItem, 0, len(metricNames))
	for _, metricName := range metricNames {
		metric, ok := a.Catalog.Metrics[metricName]
		if !ok {
			continue
		}
		metrics = append(metrics, listItem{
			Name: metricName, Type: metric.Type, Unit: metric.Unit,
			Role: metric.Role, Service: metric.Service,
			Flags: append([]string(nil), metric.Flags...),
		})
	}
	writeJSON(w, r, a.Catalog.Version, serviceResponse{
		Schema: SchemaVersion, Version: a.Catalog.Version,
		Service: name, Known: true, Description: service.Description,
		Owner: service.Owner, Runbook: service.Runbook,
		Metrics: metrics,
		Logs:    append([]string(nil), service.Logs...),
		Traces:  append([]string(nil), service.Traces...),
		HasMore: total > len(metrics),
	})
}

func parseDetail(raw string) (detailLevel, error) {
	if raw == "" {
		return detailMinimal, nil
	}
	switch detailLevel(raw) {
	case detailMinimal, detailStandard, detailFull:
		return detailLevel(raw), nil
	default:
		return "", fmt.Errorf("invalid detail %q", raw)
	}
}

func parseInclude(raw string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := allowedIncludes[part]; !ok {
			return nil, fmt.Errorf("invalid include %q", part)
		}
		out[part] = struct{}{}
	}
	return out, nil
}

func parseLimit(raw string, fallback, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > max {
		return 0, fmt.Errorf("limit must be between 1 and %d", max)
	}
	return n, nil
}

func cloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func writeJSON(w http.ResponseWriter, r *http.Request, version string, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot encode context")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Skopos-Context-Version", sanitizeHeader(version))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

func sanitizeHeader(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	return strings.ReplaceAll(value, "\n", "")
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
