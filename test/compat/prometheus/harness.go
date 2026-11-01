package promcompat

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/jawaracloud/skopos/internal/prom"
)

const lookback = 5 * time.Minute

// FakeFetcher implements prom.Fetcher over a fixed series set, mirroring the
// filtering the ClickHouse fetcher performs.
type FakeFetcher struct {
	Series []Series
}

// Fetch implements prom.Fetcher.
func (f FakeFetcher) Fetch(_ context.Context, sel prom.Selector, start, end time.Time) ([]prom.Series, error) {
	startMs, endMs := start.UnixMilli(), end.UnixMilli()
	out := make([]prom.Series, 0, len(f.Series))
	for _, series := range f.Series {
		if !selectorMatches(sel, series.Labels) {
			continue
		}
		samples := make([]prom.Sample, 0, len(series.Samples))
		for _, sample := range series.Samples {
			if sample.T >= startMs && sample.T <= endMs {
				samples = append(samples, prom.Sample{T: sample.T, V: sample.V})
			}
		}
		if len(samples) > 0 {
			out = append(out, prom.Series{Labels: cloneMap(series.Labels), Samples: samples})
		}
	}
	return out, nil
}

func selectorMatches(sel prom.Selector, lset map[string]string) bool {
	for _, matcher := range sel.Matchers {
		if !matcher.Matches(lset[matcher.Name]) {
			return false
		}
	}
	return true
}

// Engines holds both evaluators over the same data.
type Engines struct {
	Our       *prom.Engine
	Reference *promql.Engine
	refSeries []Series
}

// NewEngines builds our engine and the official Prometheus engine.
func NewEngines(data []Series) *Engines {
	ref := promql.NewEngine(promql.EngineOpts{
		MaxSamples:               500_000,
		Timeout:                  30 * time.Second,
		LookbackDelta:            lookback,
		EnableAtModifier:         false,
		EnableNegativeOffset:     false,
		EnableDelayedNameRemoval: false,
		Parser:                   parser.NewParser(parser.Options{}),
	})
	ours := prom.NewEngine(FakeFetcher{Series: data})
	ours.Lookback = lookback
	return &Engines{Our: ours, Reference: ref, refSeries: data}
}

// Close releases the reference engine.
func (e *Engines) Close() {
	_ = e.Reference.Close()
}

// Instant runs expr at one timestamp on both engines and compares the values.
func (e *Engines) Instant(ctx context.Context, expr string, at time.Time) error {
	got, gotErr := e.Our.Query(ctx, expr, at)
	ref, refErr := e.referenceInstant(ctx, expr, at)
	if gotErr != nil || refErr != nil {
		switch {
		case gotErr != nil && refErr != nil:
			return nil
		case gotErr != nil:
			return fmt.Errorf("skopos: %w (prometheus succeeded)", gotErr)
		default:
			return fmt.Errorf("prometheus: %w (skopos succeeded)", refErr)
		}
	}
	return compareInstant(got, ref)
}

func (e *Engines) referenceInstant(ctx context.Context, expr string, at time.Time) (any, error) {
	query, err := e.Reference.NewInstantQuery(ctx, NewQueryable(e.refSeries), promql.NewPrometheusQueryOpts(false, lookback), expr, at)
	if err != nil {
		return nil, err
	}
	defer query.Close()
	result := query.Exec(ctx)
	if result.Err != nil {
		return nil, result.Err
	}
	return result.Value, nil
}

// Range runs expr over [start, end] with step on both engines and compares the
// resulting series.
func (e *Engines) Range(ctx context.Context, expr string, start, end time.Time, step time.Duration) error {
	got, gotErr := e.Our.QueryRange(ctx, expr, start, end, step)
	ref, refErr := e.referenceRange(ctx, expr, start, end, step)
	if gotErr != nil || refErr != nil {
		switch {
		case gotErr != nil && refErr != nil:
			return nil
		case gotErr != nil:
			return fmt.Errorf("skopos: %w (prometheus succeeded)", gotErr)
		default:
			return fmt.Errorf("prometheus: %w (skopos succeeded)", refErr)
		}
	}
	return compareRange(got, ref)
}

func (e *Engines) referenceRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (promql.Matrix, error) {
	query, err := e.Reference.NewRangeQuery(ctx, NewQueryable(e.refSeries), promql.NewPrometheusQueryOpts(false, lookback), expr, start, end, step)
	if err != nil {
		return nil, err
	}
	defer query.Close()
	result := query.Exec(ctx)
	if result.Err != nil {
		return nil, result.Err
	}
	return result.Value.(promql.Matrix), nil
}

// SetReferenceSeries replaces the series used by the official engine's
// storage; NewEngines wires this up internally.
func (e *Engines) SetReferenceSeries(series []Series) {
	e.refSeries = series
}

func compareInstant(got prom.Value, ref any) error {
	switch value := ref.(type) {
	case promql.Scalar:
		scalar, ok := got.(prom.Scalar)
		if !ok {
			return fmt.Errorf("result type = %T, want scalar", got)
		}
		if !EqualValues(float64(scalar), value.V) {
			return fmt.Errorf("scalar = %v, want %v", scalar, value.V)
		}
		return nil
	case promql.Vector:
		vector, ok := got.(prom.Vector)
		if !ok {
			return fmt.Errorf("result type = %T, want vector", got)
		}
		return compareVectors(vector, value)
	case promql.Matrix:
		return fmt.Errorf("prometheus returned a range vector from an instant query")
	default:
		return fmt.Errorf("unexpected reference result type %T", ref)
	}
}

func compareVectors(got prom.Vector, want promql.Vector) error {
	gotByLabels := make(map[string]float64, len(got))
	for _, sample := range got {
		key := labelsKey(sample.Labels)
		if previous, exists := gotByLabels[key]; exists {
			return fmt.Errorf("duplicate result labelset %s (values %v and %v)", key, previous, sample.V)
		}
		gotByLabels[key] = sample.V
	}
	wantByLabels := make(map[string]float64, len(want))
	for _, sample := range want {
		key := labelsKey(sample.Metric.Map())
		if previous, exists := wantByLabels[key]; exists {
			return fmt.Errorf("prometheus duplicate labelset %s (values %v and %v)", key, previous, sample.F)
		}
		wantByLabels[key] = sample.F
	}
	if len(gotByLabels) != len(wantByLabels) {
		return fmt.Errorf("vector length = %d, want %d\ngot:  %v\nwant: %v", len(gotByLabels), len(wantByLabels), gotByLabels, wantByLabels)
	}
	for key, wantValue := range wantByLabels {
		gotValue, ok := gotByLabels[key]
		if !ok {
			return fmt.Errorf("missing series %s\ngot:  %v\nwant: %v", key, gotByLabels, wantByLabels)
		}
		if !EqualValues(gotValue, wantValue) {
			return fmt.Errorf("series %s value = %v, want %v", key, gotValue, wantValue)
		}
	}
	return nil
}

func compareRange(got prom.Matrix, want promql.Matrix) error {
	gotByLabels := make(map[string][]prom.Sample, len(got))
	for _, series := range got {
		gotByLabels[labelsKey(series.Labels)] = series.Samples
	}
	wantByLabels := make(map[string][]prom.Sample, len(want))
	for _, series := range want {
		samples := make([]prom.Sample, 0, len(series.Floats))
		for _, sample := range series.Floats {
			samples = append(samples, prom.Sample{T: sample.T, V: sample.F})
		}
		wantByLabels[labelsKey(series.Metric.Map())] = samples
	}
	if len(gotByLabels) != len(wantByLabels) {
		return fmt.Errorf("series count = %d, want %d\ngot:  %v\nwant: %v", len(gotByLabels), len(wantByLabels), keys(gotByLabels), keys(wantByLabels))
	}
	for key, wantSamples := range wantByLabels {
		gotSamples, ok := gotByLabels[key]
		if !ok {
			return fmt.Errorf("missing series %s\ngot:  %v\nwant: %v", key, keys(gotByLabels), keys(wantByLabels))
		}
		if len(gotSamples) != len(wantSamples) {
			return fmt.Errorf("series %s sample count = %d, want %d\ngot:  %v\nwant: %v", key, len(gotSamples), len(wantSamples), gotSamples, wantSamples)
		}
		for i := range wantSamples {
			if gotSamples[i].T != wantSamples[i].T {
				return fmt.Errorf("series %s sample %d timestamp = %d, want %d", key, i, gotSamples[i].T, wantSamples[i].T)
			}
			if !EqualValues(gotSamples[i].V, wantSamples[i].V) {
				return fmt.Errorf("series %s sample %d value = %v, want %v", key, i, gotSamples[i].V, wantSamples[i].V)
			}
		}
	}
	return nil
}

// LabelsKey is exposed for test helpers that need a stable label set key.
func LabelsKey(lset map[string]string) string { return labelsKey(lset) }

func labelsKey(lset map[string]string) string {
	names := make([]string, 0, len(lset))
	for name := range lset {
		names = append(names, name)
	}
	sort.Strings(names)
	out := "{"
	for _, name := range names {
		if out != "{" {
			out += ","
		}
		out += name + "=" + fmt.Sprintf("%q", lset[name])
	}
	return out + "}"
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// ReferenceLabels is a small helper for tests that need Prometheus labels.
func ReferenceLabels(lset map[string]string) labels.Labels { return labels.FromMap(lset) }
