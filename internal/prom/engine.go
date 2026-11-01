package prom

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// DefaultLookback is the Prometheus lookback delta for instant selectors.
const DefaultLookback = 5 * time.Minute

// MaxPoints is the default maximum number of points in a range query.
const MaxPoints = 11000

// Engine parses and evaluates the supported PromQL subset.
type Engine struct {
	Fetcher    Fetcher
	Lookback   time.Duration
	MaxSamples int
	MaxPoints  int
}

// NewEngine returns an engine with alpha defaults.
func NewEngine(fetcher Fetcher) *Engine {
	return &Engine{Fetcher: fetcher, Lookback: DefaultLookback, MaxSamples: 500_000, MaxPoints: MaxPoints}
}

// Query evaluates expr at one instant.
func (e *Engine) Query(ctx context.Context, expr string, at time.Time) (Value, error) {
	parsed, err := parser.NewParser(parser.Options{}).ParseExpr(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: parse query: %v", ErrBadQuery, err)
	}
	span := maxSelectorSpan(parsed, e.lookback())
	ec := &evalCtx{
		engine:     e,
		ctx:        ctx,
		at:         at,
		fetchStart: at.Add(-span),
		fetchEnd:   at,
		cache:      make(map[string][]Series),
	}
	v, err := ec.eval(parsed)
	if err != nil {
		return nil, err
	}
	if _, ok := v.(Matrix); ok {
		return nil, errors.New("prom: range vector is not a valid instant query result")
	}
	return v, nil
}

// QueryRange evaluates expr at each step in [start, end].
func (e *Engine) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (Matrix, error) {
	if step <= 0 {
		return nil, errors.New("prom: step must be positive")
	}
	if end.Before(start) {
		return nil, errors.New("prom: end is before start")
	}
	points := int(end.Sub(start)/step) + 1
	maxPoints := e.MaxPoints
	if maxPoints == 0 {
		maxPoints = MaxPoints
	}
	if points > maxPoints {
		return nil, fmt.Errorf("prom: query would produce %d points, maximum is %d", points, maxPoints)
	}
	parsed, err := parser.NewParser(parser.Options{}).ParseExpr(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: parse query: %v", ErrBadQuery, err)
	}
	span := maxSelectorSpan(parsed, e.lookback())
	base := &evalCtx{
		engine:     e,
		ctx:        ctx,
		fetchStart: start.Add(-span),
		fetchEnd:   end,
		cache:      make(map[string][]Series),
	}

	type accumulated struct {
		labels  map[string]string
		samples []Sample
	}
	byKey := make(map[string]*accumulated)
	for at := start; !at.After(end); at = at.Add(step) {
		ec := *base
		ec.at = at
		v, err := ec.eval(parsed)
		if err != nil {
			return nil, err
		}
		switch value := v.(type) {
		case Scalar:
			key := ""
			acc := byKey[key]
			if acc == nil {
				acc = &accumulated{labels: map[string]string{}}
				byKey[key] = acc
			}
			acc.samples = append(acc.samples, Sample{T: at.UnixMilli(), V: float64(value)})
		case Vector:
			for _, sample := range value {
				key := labelsKey(sample.Labels)
				acc := byKey[key]
				if acc == nil {
					acc = &accumulated{labels: cloneLabels(sample.Labels)}
					byKey[key] = acc
				}
				acc.samples = append(acc.samples, Sample{T: at.UnixMilli(), V: sample.V})
			}
		default:
			return nil, fmt.Errorf("prom: unsupported range result type %T", v)
		}
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(Matrix, 0, len(keys))
	for _, k := range keys {
		acc := byKey[k]
		sort.Slice(acc.samples, func(i, j int) bool { return acc.samples[i].T < acc.samples[j].T })
		out = append(out, Series{Labels: acc.labels, Samples: acc.samples})
	}
	return out, nil
}

func (e *Engine) lookback() time.Duration {
	if e.Lookback <= 0 {
		return DefaultLookback
	}
	return e.Lookback
}

type evalCtx struct {
	engine     *Engine
	ctx        context.Context
	at         time.Time
	fetchStart time.Time
	fetchEnd   time.Time
	cache      map[string][]Series
}

func (e *evalCtx) eval(expr parser.Expr) (Value, error) {
	switch ex := expr.(type) {
	case *parser.NumberLiteral:
		return Scalar(ex.Val), nil
	case *parser.ParenExpr:
		return e.eval(ex.Expr)
	case *parser.UnaryExpr:
		return e.evalUnary(ex)
	case *parser.BinaryExpr:
		return e.evalBinary(ex)
	case *parser.AggregateExpr:
		return e.evalAggregate(ex)
	case *parser.Call:
		return e.evalCall(ex)
	case *parser.VectorSelector:
		return e.evalVectorSelector(ex)
	case *parser.MatrixSelector:
		return e.evalMatrixSelector(ex)
	case *parser.SubqueryExpr:
		return nil, errors.New("prom: subqueries are not supported")
	case *parser.StepInvariantExpr:
		return e.eval(ex.Expr)
	case *parser.StringLiteral:
		return nil, errors.New("prom: string literals are not supported")
	default:
		return nil, fmt.Errorf("prom: unsupported expression %T", expr)
	}
}

func (e *evalCtx) fetch(sel Selector) ([]Series, error) {
	key := sel.String()
	if cached, ok := e.cache[key]; ok {
		return cached, nil
	}
	series, err := e.engine.Fetcher.Fetch(e.ctx, sel, e.fetchStart, e.fetchEnd)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, s := range series {
		total += len(s.Samples)
	}
	if max := e.engine.MaxSamples; max > 0 && total > max {
		return nil, fmt.Errorf("prom: selector returned %d samples, maximum is %d", total, max)
	}
	e.cache[key] = series
	return series, nil
}

func (e *evalCtx) evalVectorSelector(vs *parser.VectorSelector) (Value, error) {
	if vs.Timestamp != nil || vs.StartOrEnd != 0 {
		return nil, errors.New("prom: @ modifiers are not supported")
	}
	offset := vs.OriginalOffset
	end := e.at.Add(-offset)
	start := end.Add(-e.engine.lookback())
	series, err := e.fetch(selectorFromVector(vs))
	if err != nil {
		return nil, err
	}
	endMs := end.UnixMilli()
	startMs := start.UnixMilli()
	out := make(Vector, 0, len(series))
	for _, s := range series {
		idx := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T > endMs })
		if idx == 0 {
			continue
		}
		last := s.Samples[idx-1]
		if last.T < startMs {
			continue
		}
		out = append(out, VectorSample{Labels: cloneLabels(s.Labels), T: e.at.UnixMilli(), SampleT: last.T, V: last.V})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

func (e *evalCtx) evalMatrixSelector(ms *parser.MatrixSelector) (Value, error) {
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, errors.New("prom: matrix selector does not contain a vector selector")
	}
	if vs.Timestamp != nil || vs.StartOrEnd != 0 {
		return nil, errors.New("prom: @ modifiers are not supported")
	}
	offset := vs.OriginalOffset
	end := e.at.Add(-offset)
	start := end.Add(-ms.Range)
	series, err := e.fetch(selectorFromVector(vs))
	if err != nil {
		return nil, err
	}
	startMs := start.UnixMilli()
	endMs := end.UnixMilli()
	out := make(Matrix, 0, len(series))
	for _, s := range series {
		startIdx := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T >= startMs })
		endIdx := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T > endMs })
		if startIdx >= endIdx {
			continue
		}
		out = append(out, Series{Labels: cloneLabels(s.Labels), Samples: s.Samples[startIdx:endIdx]})
	}
	return out, nil
}

func selectorFromVector(vs *parser.VectorSelector) Selector {
	return Selector{Matchers: append([]*promlabels.Matcher(nil), vs.LabelMatchers...)}
}

func maxSelectorSpan(expr parser.Expr, lookback time.Duration) time.Duration {
	var span time.Duration
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			if d := n.OriginalOffset + lookback; d > span {
				span = d
			}
		case *parser.MatrixSelector:
			offset := time.Duration(0)
			if vs, ok := n.VectorSelector.(*parser.VectorSelector); ok {
				offset = vs.OriginalOffset
			}
			if d := offset + n.Range + lookback; d > span {
				span = d
			}
		}
		return nil
	})
	if span <= 0 {
		span = lookback
	}
	return span
}

// --- Calls ---

func (e *evalCtx) evalCall(call *parser.Call) (Value, error) {
	name := call.Func.Name
	switch name {
	case "rate", "increase", "irate":
		matrix, err := e.evalMatrixArg(call, 0)
		if err != nil {
			return nil, err
		}
		rangeStart, rangeEnd := e.matrixWindow(call)
		return e.rateLike(name, matrix, rangeStart, rangeEnd), nil
	case "avg_over_time", "min_over_time", "max_over_time", "sum_over_time",
		"count_over_time", "last_over_time", "stddev_over_time", "stdvar_over_time",
		"present_over_time":
		matrix, err := e.evalMatrixArg(call, 0)
		if err != nil {
			return nil, err
		}
		return e.overTime(name, matrix), nil
	case "quantile_over_time":
		if len(call.Args) != 2 {
			return nil, errors.New("prom: quantile_over_time expects a scalar and a range vector")
		}
		q, err := e.evalScalar(call.Args[0])
		if err != nil {
			return nil, err
		}
		matrix, err := e.evalMatrixArg(call, 1)
		if err != nil {
			return nil, err
		}
		return e.quantileOverTime(float64(q), matrix), nil
	case "histogram_quantile":
		if len(call.Args) != 2 {
			return nil, errors.New("prom: histogram_quantile expects a scalar and a vector")
		}
		q, err := e.evalScalar(call.Args[0])
		if err != nil {
			return nil, err
		}
		v, err := e.eval(call.Args[1])
		if err != nil {
			return nil, err
		}
		vec, ok := v.(Vector)
		if !ok {
			return nil, errors.New("prom: histogram_quantile expects a vector of buckets")
		}
		return histogramQuantile(float64(q), vec, e.at), nil
	case "time":
		return Scalar(float64(e.at.UnixMilli()) / 1000), nil
	case "timestamp":
		v, err := e.evalVectorArg(call, 0)
		if err != nil {
			return nil, err
		}
		out := make(Vector, 0, len(v))
		for _, s := range v {
			labels := cloneLabels(s.Labels)
			delete(labels, promlabels.MetricName)
			stamp := s.SampleT
			if stamp == 0 {
				stamp = s.T
			}
			out = append(out, VectorSample{Labels: labels, T: e.at.UnixMilli(), V: float64(stamp) / 1000})
		}
		return out, nil
	case "scalar":
		v, err := e.evalVectorArg(call, 0)
		if err != nil {
			return nil, err
		}
		if len(v) != 1 {
			return Scalar(math.NaN()), nil
		}
		return Scalar(v[0].V), nil
	case "vector":
		if len(call.Args) != 1 {
			return nil, errors.New("prom: vector expects one scalar")
		}
		s, err := e.evalScalar(call.Args[0])
		if err != nil {
			return nil, err
		}
		return Vector{{Labels: map[string]string{}, T: e.at.UnixMilli(), V: float64(s)}}, nil
	case "abs", "ceil", "floor", "sqrt", "exp", "ln", "log2", "log10":
		return e.evalUnaryMath(name, call)
	case "round":
		return e.evalRound(call)
	case "clamp", "clamp_min", "clamp_max":
		return e.evalClamp(name, call)
	case "sort", "sort_desc":
		v, err := e.evalVectorArg(call, 0)
		if err != nil {
			return nil, err
		}
		sort.Slice(v, func(i, j int) bool {
			if name == "sort_desc" {
				return v[i].V > v[j].V
			}
			return v[i].V < v[j].V
		})
		return v, nil
	default:
		return nil, fmt.Errorf("prom: unsupported function %q", name)
	}
}

func (e *evalCtx) evalMatrixArg(call *parser.Call, index int) (Matrix, error) {
	if len(call.Args) <= index {
		return nil, errors.New("prom: function is missing its range-vector argument")
	}
	v, err := e.eval(call.Args[index])
	if err != nil {
		return nil, err
	}
	matrix, ok := v.(Matrix)
	if !ok {
		return nil, errors.New("prom: function expects a range vector")
	}
	return matrix, nil
}

func (e *evalCtx) evalVectorArg(call *parser.Call, index int) (Vector, error) {
	if len(call.Args) <= index {
		return nil, errors.New("prom: function is missing its vector argument")
	}
	v, err := e.eval(call.Args[index])
	if err != nil {
		return nil, err
	}
	return vectorValue(v)
}

func (e *evalCtx) evalScalar(expr parser.Expr) (Scalar, error) {
	v, err := e.eval(expr)
	if err != nil {
		return 0, err
	}
	s, ok := v.(Scalar)
	if !ok {
		return 0, errors.New("prom: expected a scalar")
	}
	return s, nil
}

func (e *evalCtx) matrixWindow(call *parser.Call) (time.Time, time.Time) {
	if len(call.Args) == 0 {
		return e.at, e.at
	}
	arg := call.Args[0]
	for {
		paren, ok := arg.(*parser.ParenExpr)
		if !ok {
			break
		}
		arg = paren.Expr
	}
	ms, ok := arg.(*parser.MatrixSelector)
	if !ok {
		return e.at, e.at
	}
	offset := time.Duration(0)
	if vs, ok := ms.VectorSelector.(*parser.VectorSelector); ok {
		offset = vs.OriginalOffset
	}
	end := e.at.Add(-offset)
	return end.Add(-ms.Range), end
}

func (e *evalCtx) rateLike(name string, matrix Matrix, rangeStart, rangeEnd time.Time) Vector {
	out := make(Vector, 0, len(matrix))
	for _, series := range matrix {
		samples := series.Samples
		if len(samples) < 2 {
			continue
		}
		labels := cloneLabels(series.Labels)
		delete(labels, promlabels.MetricName)
		var value float64
		switch name {
		case "irate":
			prev, last := samples[len(samples)-2], samples[len(samples)-1]
			delta := last.V - prev.V
			if delta < 0 {
				delta = last.V
			}
			seconds := float64(last.T-prev.T) / 1000
			if seconds <= 0 {
				continue
			}
			value = delta / seconds
		default:
			// Counter reset correction: last-first plus each previous value
			// when the counter resets.
			increase := samples[len(samples)-1].V - samples[0].V
			for i := 1; i < len(samples); i++ {
				if samples[i].V < samples[i-1].V {
					increase += samples[i-1].V
				}
			}
			sampledInterval := float64(samples[len(samples)-1].T-samples[0].T) / 1000
			if sampledInterval <= 0 {
				continue
			}
			durationToStart := float64(samples[0].T-rangeStart.UnixMilli()) / 1000
			durationToEnd := float64(rangeEnd.UnixMilli()-samples[len(samples)-1].T) / 1000
			average := sampledInterval / float64(len(samples)-1)
			threshold := average * 1.1
			if durationToStart >= threshold {
				durationToStart = average / 2
			}
			if durationToEnd >= threshold {
				durationToEnd = average / 2
			}
			factor := (sampledInterval + durationToStart + durationToEnd) / sampledInterval
			value = increase * factor
			if name == "rate" {
				rangeSeconds := rangeEnd.Sub(rangeStart).Seconds()
				if rangeSeconds <= 0 {
					continue
				}
				value /= rangeSeconds
			}
		}
		out = append(out, VectorSample{Labels: labels, T: e.at.UnixMilli(), V: value})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out
}

func (e *evalCtx) overTime(name string, matrix Matrix) Vector {
	out := make(Vector, 0, len(matrix))
	for _, series := range matrix {
		if len(series.Samples) == 0 {
			continue
		}
		values := make([]float64, len(series.Samples))
		for i, s := range series.Samples {
			values[i] = s.V
		}
		var value float64
		switch name {
		case "avg_over_time":
			value = mean(values)
		case "min_over_time":
			value = values[0]
			for _, v := range values[1:] {
				if v < value {
					value = v
				}
			}
		case "max_over_time":
			value = values[0]
			for _, v := range values[1:] {
				if v > value {
					value = v
				}
			}
		case "sum_over_time":
			for _, v := range values {
				value += v
			}
		case "count_over_time":
			value = float64(len(values))
		case "last_over_time":
			value = values[len(values)-1]
		case "stddev_over_time":
			value = math.Sqrt(variance(values))
		case "stdvar_over_time":
			value = variance(values)
		case "present_over_time":
			value = 1
		}
		labels := cloneLabels(series.Labels)
		delete(labels, promlabels.MetricName)
		out = append(out, VectorSample{Labels: labels, T: e.at.UnixMilli(), V: value})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out
}

func (e *evalCtx) quantileOverTime(q float64, matrix Matrix) Vector {
	out := make(Vector, 0, len(matrix))
	for _, series := range matrix {
		if len(series.Samples) == 0 {
			continue
		}
		values := make([]float64, len(series.Samples))
		for i, s := range series.Samples {
			values[i] = s.V
		}
		sort.Float64s(values)
		labels := cloneLabels(series.Labels)
		delete(labels, promlabels.MetricName)
		out = append(out, VectorSample{Labels: labels, T: e.at.UnixMilli(), V: quantileSorted(q, values)})
	}
	return out
}

func (e *evalCtx) evalUnaryMath(name string, call *parser.Call) (Value, error) {
	if len(call.Args) != 1 {
		return nil, fmt.Errorf("prom: %s expects one argument", name)
	}
	v, err := e.eval(call.Args[0])
	if err != nil {
		return nil, err
	}
	fn := func(x float64) float64 {
		switch name {
		case "abs":
			return math.Abs(x)
		case "ceil":
			return math.Ceil(x)
		case "floor":
			return math.Floor(x)
		case "sqrt":
			return math.Sqrt(x)
		case "exp":
			return math.Exp(x)
		case "ln":
			return math.Log(x)
		case "log2":
			return math.Log2(x)
		case "log10":
			return math.Log10(x)
		default:
			return x
		}
	}
	switch value := v.(type) {
	case Scalar:
		return Scalar(fn(float64(value))), nil
	case Vector:
		out := make(Vector, 0, len(value))
		for _, s := range value {
			labels := cloneLabels(s.Labels)
			delete(labels, promlabels.MetricName)
			out = append(out, VectorSample{Labels: labels, T: s.T, V: fn(s.V)})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("prom: %s expects a scalar or vector", name)
	}
}

func (e *evalCtx) evalRound(call *parser.Call) (Value, error) {
	if len(call.Args) < 1 || len(call.Args) > 2 {
		return nil, errors.New("prom: round expects one or two arguments")
	}
	toNearest := 1.0
	if len(call.Args) == 2 {
		s, err := e.evalScalar(call.Args[1])
		if err != nil {
			return nil, err
		}
		toNearest = float64(s)
		if toNearest == 0 {
			toNearest = 1
		}
	}
	v, err := e.eval(call.Args[0])
	if err != nil {
		return nil, err
	}
	round := func(x float64) float64 { return math.Round(x/toNearest) * toNearest }
	switch value := v.(type) {
	case Scalar:
		return Scalar(round(float64(value))), nil
	case Vector:
		out := make(Vector, 0, len(value))
		for _, s := range value {
			labels := cloneLabels(s.Labels)
			delete(labels, promlabels.MetricName)
			out = append(out, VectorSample{Labels: labels, T: s.T, V: round(s.V)})
		}
		return out, nil
	default:
		return nil, errors.New("prom: round expects a scalar or vector")
	}
}

func (e *evalCtx) evalClamp(name string, call *parser.Call) (Value, error) {
	var min, max *float64
	switch name {
	case "clamp":
		if len(call.Args) != 3 {
			return nil, errors.New("prom: clamp expects three arguments")
		}
		lo, err := e.evalScalar(call.Args[1])
		if err != nil {
			return nil, err
		}
		hi, err := e.evalScalar(call.Args[2])
		if err != nil {
			return nil, err
		}
		min, max = ptr(float64(lo)), ptr(float64(hi))
	case "clamp_min":
		if len(call.Args) != 2 {
			return nil, errors.New("prom: clamp_min expects two arguments")
		}
		lo, err := e.evalScalar(call.Args[1])
		if err != nil {
			return nil, err
		}
		min = ptr(float64(lo))
	case "clamp_max":
		if len(call.Args) != 2 {
			return nil, errors.New("prom: clamp_max expects two arguments")
		}
		hi, err := e.evalScalar(call.Args[1])
		if err != nil {
			return nil, err
		}
		max = ptr(float64(hi))
	}
	v, err := e.eval(call.Args[0])
	if err != nil {
		return nil, err
	}
	clamp := func(x float64) float64 {
		if min != nil && x < *min {
			x = *min
		}
		if max != nil && x > *max {
			x = *max
		}
		return x
	}
	switch value := v.(type) {
	case Scalar:
		return Scalar(clamp(float64(value))), nil
	case Vector:
		out := make(Vector, 0, len(value))
		for _, s := range value {
			labels := cloneLabels(s.Labels)
			delete(labels, promlabels.MetricName)
			out = append(out, VectorSample{Labels: labels, T: s.T, V: clamp(s.V)})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("prom: %s expects a scalar or vector", name)
	}
}

func ptr(v float64) *float64 { return &v }

// --- Aggregations ---

func (e *evalCtx) evalAggregate(agg *parser.AggregateExpr) (Value, error) {
	op := agg.Op.String()
	switch op {
	case "sum", "avg", "min", "max", "count", "stddev", "stdvar", "group":
	default:
		return nil, fmt.Errorf("prom: unsupported aggregation %q", op)
	}
	v, err := e.eval(agg.Expr)
	if err != nil {
		return nil, err
	}
	vec, ok := v.(Vector)
	if !ok {
		return nil, errors.New("prom: aggregation expects a vector")
	}
	type group struct {
		labels map[string]string
		values []float64
	}
	groups := make(map[string]*group)
	for _, sample := range vec {
		labels := aggregateLabels(sample.Labels, agg.Grouping, agg.Without)
		key := labelsKey(labels)
		g := groups[key]
		if g == nil {
			g = &group{labels: labels}
			groups[key] = g
		}
		g.values = append(g.values, sample.V)
	}
	out := make(Vector, 0, len(groups))
	for _, g := range groups {
		var value float64
		switch op {
		case "sum":
			value = sum(g.values)
		case "avg":
			value = mean(g.values)
		case "min":
			value = minValue(g.values)
		case "max":
			value = maxValue(g.values)
		case "count":
			value = float64(len(g.values))
		case "stddev":
			value = math.Sqrt(variance(g.values))
		case "stdvar":
			value = variance(g.values)
		case "group":
			value = 1
		}
		out = append(out, VectorSample{Labels: g.labels, T: e.at.UnixMilli(), V: value})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

func aggregateLabels(in map[string]string, grouping []string, without bool) map[string]string {
	groupingSet := make(map[string]struct{}, len(grouping))
	for _, g := range grouping {
		groupingSet[g] = struct{}{}
	}
	out := make(map[string]string)
	for k, v := range in {
		if k == promlabels.MetricName {
			if _, keep := groupingSet[k]; without || !keep {
				continue
			}
			out[k] = v
			continue
		}
		_, inGroup := groupingSet[k]
		if without {
			if !inGroup {
				out[k] = v
			}
		} else if inGroup {
			out[k] = v
		}
	}
	return out
}

// --- Binary expressions ---

func (e *evalCtx) evalBinary(bin *parser.BinaryExpr) (Value, error) {
	op := bin.Op.String()
	switch op {
	case "and", "or", "unless":
		return nil, fmt.Errorf("prom: set operator %q is not supported", op)
	}
	lhs, err := e.eval(bin.LHS)
	if err != nil {
		return nil, err
	}
	rhs, err := e.eval(bin.RHS)
	if err != nil {
		return nil, err
	}
	switch l := lhs.(type) {
	case Scalar:
		switch r := rhs.(type) {
		case Scalar:
			v, keep, err := binaryValue(op, float64(l), float64(r), bin.ReturnBool)
			if err != nil {
				return nil, err
			}
			if !keep {
				return Scalar(math.NaN()), nil
			}
			return Scalar(v), nil
		case Vector:
			return applyScalarVector(op, float64(l), r, true, bin.ReturnBool, e.at), nil
		}
	case Vector:
		switch r := rhs.(type) {
		case Scalar:
			return applyScalarVector(op, float64(r), l, false, bin.ReturnBool, e.at), nil
		case Vector:
			return e.vectorBinary(op, l, r, bin.VectorMatching, bin.ReturnBool)
		}
	}
	return nil, fmt.Errorf("prom: unsupported binary operand types %T and %T", lhs, rhs)
}

func applyScalarVector(op string, scalar float64, vec Vector, scalarLeft, returnBool bool, at time.Time) Vector {
	out := make(Vector, 0, len(vec))
	for _, sample := range vec {
		var v float64
		var keep bool
		var err error
		if scalarLeft {
			v, keep, err = binaryValue(op, scalar, sample.V, returnBool)
		} else {
			v, keep, err = binaryValue(op, sample.V, scalar, returnBool)
		}
		if err != nil || !keep {
			continue
		}
		labels := cloneLabels(sample.Labels)
		if isArithmetic(op) || (isComparison(op) && returnBool) {
			delete(labels, promlabels.MetricName)
		}
		out = append(out, VectorSample{Labels: labels, T: at.UnixMilli(), V: v})
	}
	return out
}

func (e *evalCtx) vectorBinary(op string, lhs, rhs Vector, matching *parser.VectorMatching, returnBool bool) (Value, error) {
	if matching == nil {
		matching = &parser.VectorMatching{}
	}
	if len(matching.Include) > 0 || matching.Card != parser.CardOneToOne {
		return nil, errors.New("prom: group_left/group_right and many-to-many matching are not supported")
	}
	rightByKey := make(map[string]VectorSample, len(rhs))
	for _, s := range rhs {
		key := matchingKey(s.Labels, matching)
		if _, exists := rightByKey[key]; exists {
			return nil, errors.New("prom: many-to-many matching is not supported")
		}
		rightByKey[key] = s
	}
	out := make(Vector, 0, len(lhs))
	for _, left := range lhs {
		right, ok := rightByKey[matchingKey(left.Labels, matching)]
		if !ok {
			continue
		}
		v, keep, err := binaryValue(op, left.V, right.V, returnBool)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		labels := cloneLabels(left.Labels)
		delete(labels, promlabels.MetricName)
		out = append(out, VectorSample{Labels: labels, T: e.at.UnixMilli(), V: v})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

func matchingKey(l map[string]string, matching *parser.VectorMatching) string {
	selected := make(map[string]string)
	if matching.On {
		for _, name := range matching.MatchingLabels {
			selected[name] = l[name]
		}
	} else {
		ignore := make(map[string]struct{}, len(matching.MatchingLabels))
		for _, name := range matching.MatchingLabels {
			ignore[name] = struct{}{}
		}
		for k, v := range l {
			if k == promlabels.MetricName {
				continue
			}
			if _, skip := ignore[k]; !skip {
				selected[k] = v
			}
		}
	}
	return labelsKey(selected)
}

func isArithmetic(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%", "^", "atan2":
		return true
	}
	return false
}

func isComparison(op string) bool {
	switch op {
	case "==", "!=", ">", "<", ">=", "<=":
		return true
	}
	return false
}

func binaryValue(op string, a, b float64, returnBool bool) (float64, bool, error) {
	switch op {
	case "+":
		return a + b, true, nil
	case "-":
		return a - b, true, nil
	case "*":
		return a * b, true, nil
	case "/":
		return a / b, true, nil
	case "%":
		return math.Mod(a, b), true, nil
	case "^":
		return math.Pow(a, b), true, nil
	case "==", "!=", ">", "<", ">=", "<=":
		var ok bool
		switch op {
		case "==":
			ok = a == b
		case "!=":
			ok = a != b
		case ">":
			ok = a > b
		case "<":
			ok = a < b
		case ">=":
			ok = a >= b
		case "<=":
			ok = a <= b
		}
		if returnBool {
			if ok {
				return 1, true, nil
			}
			return 0, true, nil
		}
		return a, ok, nil
	case "atan2":
		return math.Atan2(a, b), true, nil
	default:
		return 0, false, fmt.Errorf("prom: unsupported binary operator %q", op)
	}
}

// --- Unary ---

func (e *evalCtx) evalUnary(un *parser.UnaryExpr) (Value, error) {
	v, err := e.eval(un.Expr)
	if err != nil {
		return nil, err
	}
	op := un.Op.String()
	if op != "-" {
		return nil, fmt.Errorf("prom: unsupported unary operator %q", op)
	}
	switch value := v.(type) {
	case Scalar:
		return Scalar(-float64(value)), nil
	case Vector:
		out := make(Vector, 0, len(value))
		for _, s := range value {
			labels := cloneLabels(s.Labels)
			delete(labels, promlabels.MetricName)
			out = append(out, VectorSample{Labels: labels, T: s.T, V: -s.V})
		}
		return out, nil
	default:
		return nil, errors.New("prom: unary minus expects a scalar or vector")
	}
}

// --- Helpers ---

func vectorValue(v Value) (Vector, error) {
	vec, ok := v.(Vector)
	if !ok {
		return nil, errors.New("prom: expected a vector")
	}
	return vec, nil
}

func sum(values []float64) float64 {
	var out float64
	for _, v := range values {
		out += v
	}
	return out
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	return sum(values) / float64(len(values))
}

func minValue(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	out := values[0]
	for _, v := range values[1:] {
		if v < out {
			out = v
		}
	}
	return out
}

func maxValue(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	out := values[0]
	for _, v := range values[1:] {
		if v > out {
			out = v
		}
	}
	return out
}

func variance(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	m := mean(values)
	var out float64
	for _, v := range values {
		d := v - m
		out += d * d
	}
	return out / float64(len(values))
}

func quantileSorted(q float64, values []float64) float64 {
	if len(values) == 0 || math.IsNaN(q) {
		return math.NaN()
	}
	if q < 0 {
		return math.Inf(-1)
	}
	if q > 1 {
		return math.Inf(1)
	}
	if len(values) == 1 {
		return values[0]
	}
	pos := q * float64(len(values)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if lower == upper {
		return values[lower]
	}
	weight := pos - float64(lower)
	return values[lower]*(1-weight) + values[upper]*weight
}

// histogramQuantile implements the standard Prometheus bucket interpolation
// for a vector of cumulative bucket series carrying an "le" label.
func histogramQuantile(q float64, buckets Vector, at time.Time) Vector {
	type group struct {
		labels map[string]string
		bound  float64
		count  float64
	}
	groups := make(map[string][]group)
	for _, sample := range buckets {
		le, ok := sample.Labels["le"]
		if !ok {
			continue
		}
		bound, err := strconv.ParseFloat(le, 64)
		if err != nil {
			continue
		}
		labels := cloneLabels(sample.Labels)
		delete(labels, "le")
		delete(labels, promlabels.MetricName)
		key := labelsKey(labels)
		groups[key] = append(groups[key], group{labels: labels, bound: bound, count: sample.V})
	}
	out := make(Vector, 0, len(groups))
	for _, buckets := range groups {
		if len(buckets) < 2 {
			continue
		}
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].bound < buckets[j].bound })
		total := buckets[len(buckets)-1].count
		if total <= 0 || math.IsNaN(total) {
			out = append(out, VectorSample{Labels: buckets[0].labels, T: at.UnixMilli(), V: math.NaN()})
			continue
		}
		rank := q * total
		value := math.Inf(1)
		prevBound := 0.0
		prevCount := 0.0
		for i, b := range buckets {
			if b.count >= rank {
				if math.IsInf(b.bound, 1) {
					value = prevBound
					break
				}
				denominator := b.count - prevCount
				if denominator <= 0 {
					value = prevBound
					break
				}
				if i == 0 {
					value = (rank - prevCount) / denominator * b.bound
				} else {
					span := b.bound - prevBound
					value = prevBound + (rank-prevCount)/denominator*span
				}
				break
			}
			prevBound = b.bound
			prevCount = b.count
		}
		out = append(out, VectorSample{Labels: buckets[0].labels, T: at.UnixMilli(), V: value})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out
}
