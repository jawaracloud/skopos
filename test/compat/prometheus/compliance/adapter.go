// Package compliance adapts the Skopos PromQL engine to the official
// Prometheus promqltest harness. It lets the unmodified upstream compliance
// scripts (promql/promqltest/testdata/*.test) run against the same engine the
// Skopos gateway uses for /api/v1/query and /api/v1/query_range.
package compliance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/stats"

	"github.com/jawaracloud/skopos/internal/prom"
)

// ErrNativeHistogram is returned when a fixture contains native-histogram
// samples. Skopos does not store or evaluate native histograms; returning an
// explicit error keeps the compliance result honest instead of silently
// dropping the series.
var ErrNativeHistogram = errors.New("skopos: native histogram samples are not supported")

// Engine implements promql.QueryEngine on top of the Skopos evaluator.
type Engine struct {
	// Lookback is the instant-selector lookback delta. Zero uses the Skopos
	// default.
	Lookback time.Duration
}

var _ promql.QueryEngine = Engine{}

// NewInstantQuery implements promql.QueryEngine.
func (e Engine) NewInstantQuery(_ context.Context, q storage.Queryable, _ promql.QueryOpts, qs string, ts time.Time) (promql.Query, error) {
	return &query{
		engine: e.newEngine(q),
		expr:   qs,
		start:  ts,
		end:    ts,
		step:   0,
	}, nil
}

// NewRangeQuery implements promql.QueryEngine.
func (e Engine) NewRangeQuery(_ context.Context, q storage.Queryable, _ promql.QueryOpts, qs string, start, end time.Time, interval time.Duration) (promql.Query, error) {
	return &query{
		engine: e.newEngine(q),
		expr:   qs,
		start:  start,
		end:    end,
		step:   interval,
	}, nil
}

func (e Engine) newEngine(q storage.Queryable) *prom.Engine {
	engine := prom.NewEngine(storageFetcher{queryable: q})
	if e.Lookback > 0 {
		engine.Lookback = e.Lookback
	}
	return engine
}

type query struct {
	engine *prom.Engine
	expr   string
	start  time.Time
	end    time.Time
	step   time.Duration
	stats  stats.Statistics
}

func (q *query) Exec(ctx context.Context) *promql.Result {
	if q.step > 0 {
		matrix, err := q.engine.QueryRange(ctx, q.expr, q.start, q.end, q.step)
		if err != nil {
			return &promql.Result{Err: err}
		}
		return &promql.Result{Value: toPromMatrix(matrix)}
	}
	value, err := q.engine.Query(ctx, q.expr, q.start)
	if err != nil {
		return &promql.Result{Err: err}
	}
	switch v := value.(type) {
	case prom.Scalar:
		return &promql.Result{Value: promql.Scalar{T: q.start.UnixMilli(), V: float64(v)}}
	case prom.Vector:
		return &promql.Result{Value: toPromVector(v)}
	default:
		return &promql.Result{Err: fmt.Errorf("skopos: unexpected instant result type %T", value)}
	}
}

func (q *query) Close() {}

func (q *query) Statement() parser.Statement {
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q.expr)
	if err != nil {
		return nil
	}
	return &parser.EvalStmt{Expr: expr, Start: q.start, End: q.end, Interval: q.step}
}

func (q *query) Stats() *stats.Statistics { return &q.stats }

func (q *query) Cancel() {}

func (q *query) String() string { return q.expr }

func toPromVector(vector prom.Vector) promql.Vector {
	out := make(promql.Vector, len(vector))
	for i, sample := range vector {
		out[i] = promql.Sample{
			T:      sample.T,
			F:      sample.V,
			Metric: labels.FromMap(sample.Labels),
		}
	}
	return out
}

func toPromMatrix(matrix prom.Matrix) promql.Matrix {
	out := make(promql.Matrix, len(matrix))
	for i, series := range matrix {
		floats := make([]promql.FPoint, len(series.Samples))
		for j, sample := range series.Samples {
			floats[j] = promql.FPoint{T: sample.T, F: sample.V}
		}
		out[i] = promql.Series{Metric: labels.FromMap(series.Labels), Floats: floats}
	}
	return out
}

// storageFetcher adapts a Prometheus storage.Queryable (the official test
// storage) to the Skopos prom.Fetcher interface.
type storageFetcher struct {
	queryable storage.Queryable
}

var _ prom.Fetcher = storageFetcher{}

func (f storageFetcher) Fetch(ctx context.Context, sel prom.Selector, start, end time.Time) ([]prom.Series, error) {
	querier, err := f.queryable.Querier(math.MinInt64, math.MaxInt64)
	if err != nil {
		return nil, fmt.Errorf("skopos compliance: open querier: %w", err)
	}
	defer querier.Close()

	hints := &storage.SelectHints{Start: start.UnixMilli(), End: end.UnixMilli()}
	set := querier.Select(ctx, false, hints, sel.Matchers...)

	startMs, endMs := start.UnixMilli(), end.UnixMilli()
	var out []prom.Series
	for set.Next() {
		series := set.At()
		it := series.Iterator(nil)
		var samples []prom.Sample
		for typ := it.Next(); typ != chunkenc.ValNone; typ = it.Next() {
			switch typ {
			case chunkenc.ValFloat:
				ts, value := it.At()
				if ts < startMs || ts > endMs {
					continue
				}
				samples = append(samples, prom.Sample{T: ts, V: value})
			case chunkenc.ValHistogram, chunkenc.ValFloatHistogram:
				return nil, ErrNativeHistogram
			}
		}
		if err := it.Err(); err != nil {
			return nil, fmt.Errorf("skopos compliance: iterate series: %w", err)
		}
		if len(samples) > 0 {
			out = append(out, prom.Series{Labels: series.Labels().Map(), Samples: samples})
		}
	}
	if err := set.Err(); err != nil {
		return nil, fmt.Errorf("skopos compliance: select series: %w", err)
	}
	return out, nil
}
