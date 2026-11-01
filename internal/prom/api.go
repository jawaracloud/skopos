package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// QueryEngine is the subset of Engine used by the HTTP API.
type QueryEngine interface {
	Query(ctx context.Context, expr string, at time.Time) (Value, error)
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (Matrix, error)
}

// SeriesFetcher is the storage surface used by the metadata endpoints.
type SeriesFetcher interface {
	Fetch(ctx context.Context, sel Selector, start, end time.Time) ([]Series, error)
	Metadata(ctx context.Context, metric string) (map[string]Metadata, error)
}

// API serves the Prometheus HTTP API endpoints used by Grafana.
type API struct {
	Engine   QueryEngine
	Fetcher  SeriesFetcher
	Version  string
	MaxRange time.Duration
	Now      func() time.Time
}

// NewAPI returns a Prometheus-compatible API.
func NewAPI(engine QueryEngine, fetcher SeriesFetcher, version string) *API {
	return &API{Engine: engine, Fetcher: fetcher, Version: version, MaxRange: 7 * 24 * time.Hour, Now: time.Now}
}

// Register attaches the Prometheus routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		mux.HandleFunc(method+" /api/v1/query", a.handleQuery)
		mux.HandleFunc(method+" /api/v1/query_range", a.handleQueryRange)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		mux.HandleFunc(method+" /api/v1/series", a.handleSeries)
		mux.HandleFunc(method+" /api/v1/labels", a.handleLabels)
		mux.HandleFunc(method+" /api/v1/label/{name}/values", a.handleLabelValues)
		mux.HandleFunc(method+" /api/v1/metadata", a.handleMetadata)
		mux.HandleFunc(method+" /api/v1/query_exemplars", a.handleEmptyExemplars)
	}
	mux.HandleFunc("GET /api/v1/status/buildinfo", a.handleBuildInfo)
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *API) handleQuery(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid form: "+err.Error())
		return
	}
	query := r.FormValue("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "query is required")
		return
	}
	at, err := a.parseTime(r.FormValue("time"), a.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	value, err := a.Engine.Query(r.Context(), query, at)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	data := map[string]any{"resultType": value.Type()}
	switch v := value.(type) {
	case Vector:
		data["result"] = vectorData(v)
	case Scalar:
		data["result"] = []any{float64(at.UnixMilli()) / 1000, formatFloat(float64(v))}
	default:
		writeExecutionError(w, fmt.Errorf("unsupported result type %T", value))
		return
	}
	writeSuccess(w, data)
}

func (a *API) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid form: "+err.Error())
		return
	}
	query := r.FormValue("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "query is required")
		return
	}
	start, err := a.parseRequiredTime(r.FormValue("start"), "start")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	end, err := a.parseRequiredTime(r.FormValue("end"), "end")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if end.Before(start) {
		writeError(w, http.StatusBadRequest, "bad_data", "end is before start")
		return
	}
	if a.MaxRange > 0 && end.Sub(start) > a.MaxRange {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("query range exceeds %s", a.MaxRange))
		return
	}
	step, err := parseStep(r.FormValue("step"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	matrix, err := a.Engine.QueryRange(r.Context(), query, start, end, step)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	result := make([]any, 0, len(matrix))
	for _, series := range matrix {
		values := make([]any, 0, len(series.Samples))
		for _, sample := range series.Samples {
			values = append(values, []any{float64(sample.T) / 1000, formatFloat(sample.V)})
		}
		result = append(result, map[string]any{"metric": series.Labels, "values": values})
	}
	writeSuccess(w, map[string]any{"resultType": "matrix", "result": result})
}

func (a *API) handleSeries(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid form: "+err.Error())
		return
	}
	start, end, err := a.rangeFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	selectors, err := parseSelectors(r.Form["match[]"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if len(selectors) == 0 {
		writeError(w, http.StatusBadRequest, "bad_data", "match[] is required")
		return
	}
	seen := make(map[string]map[string]string)
	for _, sel := range selectors {
		series, err := a.Fetcher.Fetch(r.Context(), sel, start, end)
		if err != nil {
			writeExecutionError(w, err)
			return
		}
		for _, s := range series {
			seen[labelsKey(s.Labels)] = s.Labels
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, seen[k])
	}
	writeSuccess(w, out)
}

func (a *API) handleLabels(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid form: "+err.Error())
		return
	}
	start, end, err := a.rangeFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	selectors, err := parseSelectors(r.Form["match[]"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if len(selectors) == 0 {
		selectors = []Selector{{}}
	}
	names := make(map[string]struct{})
	for _, sel := range selectors {
		series, err := a.Fetcher.Fetch(r.Context(), sel, start, end)
		if err != nil {
			writeExecutionError(w, err)
			return
		}
		for _, s := range series {
			for name := range s.Labels {
				names[name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	writeSuccess(w, out)
}

func (a *API) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid form: "+err.Error())
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "label name is required")
		return
	}
	start, end, err := a.rangeFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	selectors, err := parseSelectors(r.Form["match[]"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if len(selectors) == 0 {
		selectors = []Selector{{}}
	}
	values := make(map[string]struct{})
	for _, sel := range selectors {
		series, err := a.Fetcher.Fetch(r.Context(), sel, start, end)
		if err != nil {
			writeExecutionError(w, err)
			return
		}
		for _, s := range series {
			if v, ok := s.Labels[name]; ok && v != "" {
				values[v] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Strings(out)
	writeSuccess(w, out)
}

func (a *API) handleMetadata(w http.ResponseWriter, r *http.Request) {
	metric := r.FormValue("metric")
	metadata, err := a.Fetcher.Metadata(r.Context(), metric)
	if err != nil {
		writeExecutionError(w, err)
		return
	}
	out := make(map[string][]map[string]string, len(metadata))
	for name, item := range metadata {
		out[name] = []map[string]string{{"type": item.Type, "help": item.Help, "unit": item.Unit}}
	}
	writeSuccess(w, out)
}

func (a *API) handleEmptyExemplars(w http.ResponseWriter, r *http.Request) {
	writeSuccess(w, []any{})
}

func (a *API) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	writeSuccess(w, map[string]string{
		"version":   a.Version,
		"revision":  "",
		"branch":    "",
		"buildUser": "",
		"buildDate": "",
		"goVersion": runtime.Version(),
	})
}

func (a *API) rangeFromRequest(r *http.Request) (time.Time, time.Time, error) {
	now := a.now()
	start, err := a.parseTime(r.FormValue("start"), now.Add(-time.Hour))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	end, err := a.parseTime(r.FormValue("end"), now)
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

func (a *API) parseRequiredTime(value, name string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("%s is required", name)
	}
	t, err := a.parseTime(value, time.Time{})
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", name, err)
	}
	return t, nil
}

func (a *API) parseTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
	}
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC(), nil
}

func parseStep(value string) (time.Duration, error) {
	if value == "" {
		return 0, fmt.Errorf("step is required")
	}
	if d, err := time.ParseDuration(value); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("step must be positive")
		}
		return d, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf("invalid step %q", value)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

const maxSelectors = 20

func parseSelectors(values []string) ([]Selector, error) {
	if len(values) > maxSelectors {
		return nil, fmt.Errorf("too many match[] selectors: maximum is %d", maxSelectors)
	}
	out := make([]Selector, 0, len(values))
	for _, value := range values {
		matchers, err := parser.NewParser(parser.Options{}).ParseMetricSelector(value)
		if err != nil {
			return nil, fmt.Errorf("invalid match[] %q: %w", value, err)
		}
		out = append(out, Selector{Matchers: matchers})
	}
	return out, nil
}

func vectorData(vector Vector) []any {
	out := make([]any, 0, len(vector))
	for _, sample := range vector {
		out = append(out, map[string]any{
			"metric": sample.Labels,
			"value":  []any{float64(sample.T) / 1000, formatFloat(sample.V)},
		})
	}
	return out
}

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

func writeSuccess(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "error",
		"errorType": errorType,
		"error":     message,
	})
}

func writeEngineError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrBadQuery) || strings.Contains(err.Error(), "unsupported") {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	writeExecutionError(w, err)
}

func writeExecutionError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusInternalServerError, "execution", err.Error())
}
