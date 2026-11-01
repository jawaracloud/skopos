// Package promcompat is the differential harness module for the Skopos PromQL
// head. It links the official Prometheus promql evaluator and feeds it the same
// deterministic series that are loaded into internal/prom.
package promcompat

import (
	"context"
	"math"
	"sort"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"
)

// Sample is a float sample loaded into the reference storage.
type Sample struct {
	T int64
	V float64
}

// Series is a labelled series loaded into the reference storage.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

type referenceSample struct {
	t int64
	v float64
}

func (s referenceSample) T() int64                      { return s.t }
func (s referenceSample) ST() int64                     { return 0 }
func (s referenceSample) F() float64                    { return s.v }
func (s referenceSample) H() *histogram.Histogram       { return nil }
func (s referenceSample) FH() *histogram.FloatHistogram { return nil }
func (s referenceSample) Type() chunkenc.ValueType      { return chunkenc.ValFloat }
func (s referenceSample) Copy() chunks.Sample           { return s }

// Queryable is an in-memory storage.Queryable. It exists so the harness can
// use the official evaluator without pulling in a TSDB or a test-only package.
type Queryable struct {
	series []Series
}

// NewQueryable copies series into a reference queryable.
func NewQueryable(series []Series) *Queryable {
	return &Queryable{series: series}
}

func (q *Queryable) Querier(mint, maxt int64) (storage.Querier, error) {
	return &querier{series: q.series, mint: mint, maxt: maxt}, nil
}

type querier struct {
	series     []Series
	mint, maxt int64
}

func (*querier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*querier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*querier) Close() error { return nil }

func (q *querier) Select(_ context.Context, sortSeries bool, _ *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	result := make([]storage.Series, 0, len(q.series))
	for _, series := range q.series {
		if !matches(matchers, series.Labels) {
			continue
		}
		samples := make([]chunks.Sample, 0, len(series.Samples))
		for _, sample := range series.Samples {
			if sample.T < q.mint || sample.T > q.maxt {
				continue
			}
			samples = append(samples, referenceSample{t: sample.T, v: sample.V})
		}
		if len(samples) == 0 {
			continue
		}
		result = append(result, storage.NewListSeries(labels.FromMap(series.Labels), samples))
	}
	if sortSeries {
		sort.Slice(result, func(i, j int) bool {
			return labels.Compare(result[i].Labels(), result[j].Labels()) < 0
		})
	}
	return &seriesSet{series: result}
}

func matches(matchers []*labels.Matcher, lset map[string]string) bool {
	for _, matcher := range matchers {
		if !matcher.Matches(lset[matcher.Name]) {
			return false
		}
	}
	return true
}

type seriesSet struct {
	series []storage.Series
	index  int
}

func (s *seriesSet) Next() bool {
	s.index++
	return s.index <= len(s.series)
}

func (s *seriesSet) At() storage.Series {
	if s.index == 0 || s.index > len(s.series) {
		return nil
	}
	return s.series[s.index-1]
}

func (*seriesSet) Err() error                        { return nil }
func (*seriesSet) Warnings() annotations.Annotations { return nil }

// EqualValues compares floats the way the harness compares query results:
// NaNs are equal to each other, infinities must have the same sign, and
// finite values must agree within a small relative tolerance.
func EqualValues(a, b float64) bool {
	switch {
	case math.IsNaN(a) && math.IsNaN(b):
		return true
	case math.IsInf(a, 0) || math.IsInf(b, 0):
		return a == b
	}
	diff := math.Abs(a - b)
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return diff <= 1e-9*scale
}
