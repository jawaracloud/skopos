package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LogFetcher is the storage surface used by the Loki API.
type LogFetcher interface {
	FetchRange(ctx context.Context, start, end time.Time) ([]LogEntry, error)
}

// API serves the Loki-compatible endpoints used by Grafana.
type API struct {
	Fetcher  LogFetcher
	Version  string
	MaxRange time.Duration
	Now      func() time.Time
}

// NewAPI returns a Loki-compatible API.
func NewAPI(fetcher LogFetcher, version string) *API {
	return &API{Fetcher: fetcher, Version: version, MaxRange: 7 * 24 * time.Hour, Now: time.Now}
}

// Register attaches the Loki routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		mux.HandleFunc(method+" /loki/api/v1/query", a.handleQuery)
		mux.HandleFunc(method+" /loki/api/v1/query_range", a.handleQuery)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		mux.HandleFunc(method+" /loki/api/v1/labels", a.handleLabels)
		mux.HandleFunc(method+" /loki/api/v1/label/{name}/values", a.handleLabelValues)
		mux.HandleFunc(method+" /loki/api/v1/series", a.handleSeries)
	}
	mux.HandleFunc("GET /loki/api/v1/status/buildinfo", a.handleBuildInfo)
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *API) handleQuery(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
		return
	}
	queryText := r.FormValue("query")
	if queryText == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	query, err := Parse(queryText)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	start, end, err := a.timeRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entries, err := a.Fetcher.FetchRange(r.Context(), start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	limit := parseLimit(r.FormValue("limit"), 100, 5000)
	direction := strings.ToLower(r.FormValue("direction"))
	filtered := make([]LogEntry, 0, len(entries))
	for _, entry := range entries {
		if query.Match(entry) {
			filtered = append(filtered, entry)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if direction == "forward" {
			return filtered[i].Timestamp.Before(filtered[j].Timestamp)
		}
		return filtered[i].Timestamp.After(filtered[j].Timestamp)
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	type stream struct {
		labels map[string]string
		values [][]string
	}
	streams := make(map[string]*stream)
	order := make([]string, 0)
	for _, entry := range filtered {
		key := labelsKey(entry.Labels)
		s := streams[key]
		if s == nil {
			s = &stream{labels: entry.Labels}
			streams[key] = s
			order = append(order, key)
		}
		s.values = append(s.values, []string{strconv.FormatInt(entry.Timestamp.UnixNano(), 10), entry.Body})
	}
	result := make([]map[string]any, 0, len(order))
	for _, key := range order {
		s := streams[key]
		result = append(result, map[string]any{"stream": s.labels, "values": s.values})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "streams",
			"result":     result,
			"stats":      map[string]any{},
		},
	})
}

func (a *API) handleLabels(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
		return
	}
	start, end, err := a.timeRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	selectors, err := parseSelectors(r.Form["match[]"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(selectors) == 0 {
		selectors = []Query{{}}
	}
	entries, err := a.Fetcher.FetchRange(r.Context(), start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := make(map[string]struct{})
	for _, entry := range entries {
		if !matchesAny(selectors, entry) {
			continue
		}
		for name := range entry.Labels {
			names[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": out})
}

func (a *API) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "label name is required")
		return
	}
	start, end, err := a.timeRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	selectors, err := parseSelectors(r.Form["match[]"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(selectors) == 0 {
		selectors = []Query{{}}
	}
	entries, err := a.Fetcher.FetchRange(r.Context(), start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	values := make(map[string]struct{})
	for _, entry := range entries {
		if !matchesAny(selectors, entry) {
			continue
		}
		if value, ok := entry.Labels[name]; ok && value != "" {
			values[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": out})
}

func (a *API) handleSeries(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
		return
	}
	start, end, err := a.timeRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	selectors, err := parseSelectors(r.Form["match[]"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(selectors) == 0 {
		writeError(w, http.StatusBadRequest, "match[] is required")
		return
	}
	entries, err := a.Fetcher.FetchRange(r.Context(), start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	seen := make(map[string]map[string]string)
	for _, entry := range entries {
		if !matchesAny(selectors, entry) {
			continue
		}
		seen[labelsKey(entry.Labels)] = entry.Labels
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, seen[key])
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": out})
}

func (a *API) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":   a.Version,
		"revision":  "",
		"branch":    "",
		"buildDate": "",
		"buildUser": "",
		"goVersion": runtime.Version(),
	})
}

func (a *API) timeRange(r *http.Request) (time.Time, time.Time, error) {
	now := a.now()
	start, err := parseLokiTime(r.FormValue("start"), now.Add(-time.Hour))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	end, err := parseLokiTime(r.FormValue("end"), now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end is before start")
	}
	if a.MaxRange > 0 && end.Sub(start) > a.MaxRange {
		return time.Time{}, time.Time{}, fmt.Errorf("query range exceeds %s", a.MaxRange)
	}
	return start, end, nil
}

func parseLokiTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	if strings.ContainsAny(value, ".eE") {
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
		}
		whole, frac := math.Modf(seconds)
		return time.Unix(int64(whole), int64(frac*1e9)).UTC(), nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
	}
	switch {
	case len(value) <= 10:
		return time.Unix(n, 0).UTC(), nil
	case len(value) <= 13:
		return time.UnixMilli(n).UTC(), nil
	case len(value) <= 16:
		return time.UnixMicro(n).UTC(), nil
	default:
		return time.Unix(0, n).UTC(), nil
	}
}

func parseSelectors(values []string) ([]Query, error) {
	out := make([]Query, 0, len(values))
	for _, value := range values {
		query, err := Parse(value)
		if err != nil {
			return nil, err
		}
		out = append(out, query)
	}
	return out, nil
}

func matchesAny(selectors []Query, entry LogEntry) bool {
	for _, selector := range selectors {
		if selector.Match(entry) {
			return true
		}
	}
	return false
}

func labelsKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(labels[key])
		b.WriteByte(0)
	}
	return b.String()
}

func parseLimit(value string, fallback, max int) int {
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"status":    "error",
		"errorType": "bad_data",
		"error":     message,
	})
}
