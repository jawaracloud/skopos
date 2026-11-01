// Package semctx implements the optional, compact semantic context catalog and
// its read-only HTTP API. The catalog is operator-authored JSON; the package
// never invents a description, role, or unit for an unknown signal.
package semctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

// SchemaVersion is the only accepted catalog schema.
const SchemaVersion = "ctx/v1"

const (
	maxCatalogBytes = 1 << 20
	maxMetrics      = 5000
	maxServices     = 1000
	maxDescription  = 160
	maxCaveat       = 160
	maxCaveats      = 2
	maxLabels       = 20
	maxLabelValue   = 160
	maxQuery        = 512
	maxRunbook      = 300
	maxRelated      = 10
	maxFlags        = 8
	maxOwner        = 64
	maxUnit         = 32
	maxVersion      = 64
	maxMetricName   = 255
	maxServiceName  = 128
	maxServiceLinks = 20
	maxServiceItems = 50
	maxLinkText     = 200
	maxLogs         = 5000
	maxTraces       = 5000
	maxLogFields    = 20
	maxSeverity     = 6
	maxDownstream   = 20
)

var (
	metricNameRE  = regexp.MustCompile(`^[A-Za-z_:][A-Za-z0-9_:]*$`)
	labelNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	flagRE        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	versionRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	serviceNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	contextIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Catalog is the operator-authored semantic context. The source of truth is
// explicit catalog data; no field is inferred by the gateway.
type Catalog struct {
	Schema   string                  `json:"schema"`
	Version  string                  `json:"version"`
	Metrics  map[string]Metric       `json:"metrics,omitempty"`
	Services map[string]Service      `json:"services,omitempty"`
	Logs     map[string]LogContext   `json:"logs,omitempty"`
	Traces   map[string]TraceContext `json:"traces,omitempty"`
}

// Metric is one explicit metric context entry.
type Metric struct {
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Unit        string            `json:"unit"`
	Role        string            `json:"role"`
	Direction   string            `json:"direction"`
	Owner       string            `json:"owner,omitempty"`
	Service     string            `json:"service,omitempty"`
	Query       string            `json:"query,omitempty"`
	Runbook     string            `json:"runbook,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Related     []string          `json:"related,omitempty"`
	Caveats     []string          `json:"caveats,omitempty"`
	Flags       []string          `json:"flags,omitempty"`
}

// Service is one explicit service context entry.
type Service struct {
	Description string   `json:"description"`
	Owner       string   `json:"owner,omitempty"`
	Runbook     string   `json:"runbook,omitempty"`
	Metrics     []string `json:"metrics,omitempty"`
	Logs        []string `json:"logs,omitempty"`
	Traces      []string `json:"traces,omitempty"`
}

// LogContext describes one operator-defined log stream/event family.
type LogContext struct {
	Description string            `json:"description"`
	Service     string            `json:"service"`
	Event       string            `json:"event,omitempty"`
	Severity    []string          `json:"severity,omitempty"`
	Query       string            `json:"query,omitempty"`
	Runbook     string            `json:"runbook,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
	Caveats     []string          `json:"caveats,omitempty"`
	Flags       []string          `json:"flags,omitempty"`
}

// TraceContext describes one operator-defined service operation.
type TraceContext struct {
	Description  string   `json:"description"`
	Service      string   `json:"service"`
	Operation    string   `json:"operation"`
	Kind         string   `json:"kind,omitempty"`
	CriticalPath bool     `json:"critical_path,omitempty"`
	Downstream   []string `json:"downstream,omitempty"`
	Query        string   `json:"query,omitempty"`
	Runbook      string   `json:"runbook,omitempty"`
	Caveats      []string `json:"caveats,omitempty"`
	Flags        []string `json:"flags,omitempty"`
}

// EmptyCatalog returns a valid empty catalog. It is used when no context file
// is configured; the API still answers with known:false.
func EmptyCatalog() *Catalog {
	return &Catalog{Schema: SchemaVersion, Version: "empty"}
}

// LoadFile loads and validates a catalog. An empty path returns EmptyCatalog.
func LoadFile(path string) (*Catalog, error) {
	if path == "" {
		return EmptyCatalog(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("semctx: read catalog: %w", err)
	}
	catalog, err := Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("semctx: parse catalog: %w", err)
	}
	return catalog, nil
}

// Parse reads a bounded JSON catalog, rejects duplicate object keys and unknown
// fields, then validates every entry.
func Parse(r io.Reader) (*Catalog, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(data) > maxCatalogBytes {
		return nil, fmt.Errorf("catalog exceeds %d bytes", maxCatalogBytes)
	}
	if err := checkDuplicateKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var catalog Catalog
	if err := dec.Decode(&catalog); err != nil {
		return nil, err
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return &catalog, nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("catalog contains more than one JSON value")
		}
		return err
	}
	return nil
}

// Validate checks every catalog invariant. It is deliberately strict so an
// invalid context fails at startup instead of reaching an AI with half-truths.
func (c *Catalog) Validate() error {
	if c == nil {
		return errors.New("catalog is nil")
	}
	if c.Schema != SchemaVersion {
		return fmt.Errorf("schema must be %q, got %q", SchemaVersion, c.Schema)
	}
	if !versionRE.MatchString(c.Version) {
		return fmt.Errorf("version %q is invalid", c.Version)
	}
	if len(c.Metrics) > maxMetrics {
		return fmt.Errorf("too many metrics: %d > %d", len(c.Metrics), maxMetrics)
	}
	if len(c.Services) > maxServices {
		return fmt.Errorf("too many services: %d > %d", len(c.Services), maxServices)
	}
	if len(c.Logs) > maxLogs {
		return fmt.Errorf("too many logs: %d > %d", len(c.Logs), maxLogs)
	}
	if len(c.Traces) > maxTraces {
		return fmt.Errorf("too many traces: %d > %d", len(c.Traces), maxTraces)
	}

	metricNames := make([]string, 0, len(c.Metrics))
	for name := range c.Metrics {
		metricNames = append(metricNames, name)
	}
	sort.Strings(metricNames)
	for _, name := range metricNames {
		if err := validateMetricName(name); err != nil {
			return err
		}
		if err := validateMetric(c.Metrics[name], c.Services); err != nil {
			return fmt.Errorf("metric %q: %w", name, err)
		}
	}

	serviceNames := make([]string, 0, len(c.Services))
	for name := range c.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, name := range serviceNames {
		if !serviceNameRE.MatchString(name) {
			return fmt.Errorf("service name %q is invalid", name)
		}
		if err := validateService(c.Services[name], c.Metrics); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
	}

	for _, name := range metricNames {
		metric := c.Metrics[name]
		for _, related := range metric.Related {
			if _, ok := c.Metrics[related]; !ok {
				return fmt.Errorf("metric %q: related metric %q is not in the catalog", name, related)
			}
		}
		if metric.Service != "" {
			if _, ok := c.Services[metric.Service]; !ok {
				return fmt.Errorf("metric %q: service %q is not in the catalog", name, metric.Service)
			}
		}
	}

	logNames := make([]string, 0, len(c.Logs))
	for id := range c.Logs {
		logNames = append(logNames, id)
	}
	sort.Strings(logNames)
	for _, id := range logNames {
		if !contextIDRE.MatchString(id) {
			return fmt.Errorf("log context id %q is invalid", id)
		}
		if err := validateLogContext(c.Logs[id], c.Services); err != nil {
			return fmt.Errorf("log context %q: %w", id, err)
		}
	}
	traceNames := make([]string, 0, len(c.Traces))
	for id := range c.Traces {
		traceNames = append(traceNames, id)
	}
	sort.Strings(traceNames)
	for _, id := range traceNames {
		if !contextIDRE.MatchString(id) {
			return fmt.Errorf("trace context id %q is invalid", id)
		}
		if err := validateTraceContext(c.Traces[id], c.Services); err != nil {
			return fmt.Errorf("trace context %q: %w", id, err)
		}
	}
	return nil
}

func validateMetricName(name string) error {
	if len(name) > maxMetricName || !metricNameRE.MatchString(name) {
		return fmt.Errorf("metric name %q is invalid", name)
	}
	return nil
}

func validateMetric(m Metric, services map[string]Service) error {
	if err := requireText("description", m.Description, maxDescription); err != nil {
		return err
	}
	if !oneOf(m.Type, "counter", "gauge", "histogram", "summary") {
		return fmt.Errorf("type %q is invalid", m.Type)
	}
	if err := requireText("unit", m.Unit, maxUnit); err != nil {
		return err
	}
	if !oneOf(m.Role, "latency", "traffic", "error_rate", "saturation", "resource", "business_kpi", "build_info") {
		return fmt.Errorf("role %q is invalid", m.Role)
	}
	if !oneOf(m.Direction, "higher_is_worse", "lower_is_worse", "neutral") {
		return fmt.Errorf("direction %q is invalid", m.Direction)
	}
	if m.Owner != "" {
		if err := requireText("owner", m.Owner, maxOwner); err != nil {
			return err
		}
	}
	if m.Service != "" {
		if !serviceNameRE.MatchString(m.Service) {
			return fmt.Errorf("service %q is invalid", m.Service)
		}
		if _, ok := services[m.Service]; !ok {
			return fmt.Errorf("service %q is not in the catalog", m.Service)
		}
	}
	if m.Query != "" {
		if len(m.Query) > maxQuery || strings.ContainsAny(m.Query, "\r\n") {
			return fmt.Errorf("query is too long or contains a newline")
		}
	}
	if m.Runbook != "" {
		if err := validateURL("runbook", m.Runbook); err != nil {
			return err
		}
	}
	if len(m.Labels) > maxLabels {
		return fmt.Errorf("too many labels: %d > %d", len(m.Labels), maxLabels)
	}
	for key, value := range m.Labels {
		if !labelNameRE.MatchString(key) {
			return fmt.Errorf("label name %q is invalid", key)
		}
		if err := requireText("label "+key, value, maxLabelValue); err != nil {
			return err
		}
	}
	if len(m.Related) > maxRelated {
		return fmt.Errorf("too many related metrics: %d > %d", len(m.Related), maxRelated)
	}
	for _, related := range m.Related {
		if err := validateMetricName(related); err != nil {
			return fmt.Errorf("related metric: %w", err)
		}
	}
	if len(m.Caveats) > maxCaveats {
		return fmt.Errorf("too many caveats: %d > %d", len(m.Caveats), maxCaveats)
	}
	for _, caveat := range m.Caveats {
		if err := requireText("caveat", caveat, maxCaveat); err != nil {
			return err
		}
	}
	return validateFlags(m.Flags)
}

func validateFlags(flags []string) error {
	if len(flags) > maxFlags {
		return fmt.Errorf("too many flags: %d > %d", len(flags), maxFlags)
	}
	seen := make(map[string]struct{}, len(flags))
	for _, flag := range flags {
		if !flagRE.MatchString(flag) {
			return fmt.Errorf("flag %q is invalid", flag)
		}
		if _, ok := seen[flag]; ok {
			return fmt.Errorf("duplicate flag %q", flag)
		}
		seen[flag] = struct{}{}
	}
	return nil
}

func validateQuery(query string) error {
	if query != "" && (len(query) > maxQuery || strings.ContainsAny(query, "\r\n")) {
		return fmt.Errorf("query is too long or contains a newline")
	}
	return nil
}

func validateLogContext(l LogContext, services map[string]Service) error {
	if err := requireText("description", l.Description, maxDescription); err != nil {
		return err
	}
	if !serviceNameRE.MatchString(l.Service) {
		return fmt.Errorf("service %q is invalid", l.Service)
	}
	if _, ok := services[l.Service]; !ok {
		return fmt.Errorf("service %q is not in the catalog", l.Service)
	}
	if l.Event != "" && (len(l.Event) > maxMetricName || strings.TrimSpace(l.Event) != l.Event) {
		return fmt.Errorf("event is invalid")
	}
	if len(l.Severity) > maxSeverity {
		return fmt.Errorf("too many severity values")
	}
	for _, severity := range l.Severity {
		if !oneOf(severity, "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL") {
			return fmt.Errorf("severity %q is invalid", severity)
		}
	}
	if err := validateQuery(l.Query); err != nil {
		return err
	}
	if l.Runbook != "" {
		if err := validateURL("runbook", l.Runbook); err != nil {
			return err
		}
	}
	if len(l.Fields) > maxLogFields {
		return fmt.Errorf("too many fields: %d > %d", len(l.Fields), maxLogFields)
	}
	for key, value := range l.Fields {
		if !labelNameRE.MatchString(key) {
			return fmt.Errorf("field name %q is invalid", key)
		}
		if err := requireText("field "+key, value, maxLabelValue); err != nil {
			return err
		}
	}
	if len(l.Caveats) > maxCaveats {
		return fmt.Errorf("too many caveats")
	}
	for _, caveat := range l.Caveats {
		if err := requireText("caveat", caveat, maxCaveat); err != nil {
			return err
		}
	}
	return validateFlags(l.Flags)
}

func validateTraceContext(t TraceContext, services map[string]Service) error {
	if err := requireText("description", t.Description, maxDescription); err != nil {
		return err
	}
	if !serviceNameRE.MatchString(t.Service) {
		return fmt.Errorf("service %q is invalid", t.Service)
	}
	if _, ok := services[t.Service]; !ok {
		return fmt.Errorf("service %q is not in the catalog", t.Service)
	}
	if err := requireText("operation", t.Operation, maxMetricName); err != nil {
		return err
	}
	if t.Kind != "" && !oneOf(t.Kind, "server", "client", "producer", "consumer", "internal", "unspecified") {
		return fmt.Errorf("kind %q is invalid", t.Kind)
	}
	if len(t.Downstream) > maxDownstream {
		return fmt.Errorf("too many downstream services")
	}
	for _, service := range t.Downstream {
		if !serviceNameRE.MatchString(service) {
			return fmt.Errorf("downstream service %q is invalid", service)
		}
	}
	if err := validateQuery(t.Query); err != nil {
		return err
	}
	if t.Runbook != "" {
		if err := validateURL("runbook", t.Runbook); err != nil {
			return err
		}
	}
	if len(t.Caveats) > maxCaveats {
		return fmt.Errorf("too many caveats")
	}
	for _, caveat := range t.Caveats {
		if err := requireText("caveat", caveat, maxCaveat); err != nil {
			return err
		}
	}
	return validateFlags(t.Flags)
}

func validateService(s Service, metrics map[string]Metric) error {
	if err := requireText("description", s.Description, maxDescription); err != nil {
		return err
	}
	if s.Owner != "" {
		if err := requireText("owner", s.Owner, maxOwner); err != nil {
			return err
		}
	}
	if s.Runbook != "" {
		if err := validateURL("runbook", s.Runbook); err != nil {
			return err
		}
	}
	if len(s.Metrics) > maxServiceItems {
		return fmt.Errorf("too many service metrics: %d > %d", len(s.Metrics), maxServiceItems)
	}
	for _, name := range s.Metrics {
		if err := validateMetricName(name); err != nil {
			return fmt.Errorf("service metric: %w", err)
		}
		if _, ok := metrics[name]; !ok {
			return fmt.Errorf("metric %q is not in the catalog", name)
		}
	}
	for name, values := range map[string][]string{"logs": s.Logs, "traces": s.Traces} {
		if len(values) > maxServiceLinks {
			return fmt.Errorf("too many %s: %d > %d", name, len(values), maxServiceLinks)
		}
		for _, value := range values {
			if err := requireText(name, value, maxLinkText); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireText(field, value string, max int) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", field, max)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s has leading or trailing whitespace", field)
	}
	return nil
}

func validateURL(field, value string) error {
	if len(value) > maxRunbook {
		return fmt.Errorf("%s exceeds %d bytes", field, maxRunbook)
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s is not a URL: %w", field, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL", field)
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// checkDuplicateKeys walks the JSON token stream and rejects duplicate object
// keys. encoding/json would silently keep the last value, which is exactly the
// kind of ambiguous context this layer must not expose.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walkJSON(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return errors.New("catalog contains more than one JSON value")
		}
		return err
	}
	return nil
}

func walkJSON(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSON(dec); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := walkJSON(dec); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	default:
		return nil
	}
}
