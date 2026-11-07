package prom

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
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
	fetchStart, fetchEnd := selectorWindow(parsed, e.lookback(), at, at, at)
	ec := &evalCtx{
		engine:     e,
		ctx:        ctx,
		at:         at,
		queryStart: at,
		queryEnd:   at,
		fetchStart: fetchStart,
		fetchEnd:   fetchEnd,
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
	fetchStart, fetchEnd := selectorWindow(parsed, e.lookback(), start, end, start)
	base := &evalCtx{
		engine:     e,
		ctx:        ctx,
		queryStart: start,
		queryEnd:   end,
		fetchStart: fetchStart,
		fetchEnd:   fetchEnd,
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
	queryStart time.Time
	queryEnd   time.Time
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

// selectorTime returns the timestamp at which a vector selector is evaluated.
// It honours the @ modifier, including @ start() and @ end(), and defaults to
// the current evaluation step. The offset is applied by the caller relative to
// this timestamp.
func (e *evalCtx) selectorTime(vs *parser.VectorSelector) time.Time {
	switch vs.StartOrEnd {
	case parser.START:
		return e.queryStart
	case parser.END:
		return e.queryEnd
	}
	if vs.Timestamp != nil {
		return time.UnixMilli(*vs.Timestamp)
	}
	return e.at
}

func (e *evalCtx) evalVectorSelector(vs *parser.VectorSelector) (Value, error) {
	offset := vs.OriginalOffset
	end := e.selectorTime(vs).Add(-offset)
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
		if last.T <= startMs {
			continue
		}
		out = append(out, VectorSample{Labels: cloneLabels(s.Labels), T: e.at.UnixMilli(), SampleT: last.T, SampleTValid: true, V: last.V})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

func (e *evalCtx) evalMatrixSelector(ms *parser.MatrixSelector) (Value, error) {
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, errors.New("prom: matrix selector does not contain a vector selector")
	}
	offset := vs.OriginalOffset
	end := e.selectorTime(vs).Add(-offset)
	start := end.Add(-ms.Range)
	series, err := e.fetch(selectorFromVector(vs))
	if err != nil {
		return nil, err
	}
	startMs := start.UnixMilli()
	endMs := end.UnixMilli()
	out := make(Matrix, 0, len(series))
	for _, s := range series {
		// Prometheus range selectors are left-open: the sample exactly at
		// evaluation time minus the range is excluded.
		startIdx := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T > startMs })
		endIdx := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T > endMs })
		if startIdx >= endIdx {
			continue
		}
		out = append(out, Series{Labels: cloneLabels(s.Labels), Samples: s.Samples[startIdx:endIdx]})
	}
	return out, nil
}

// selectorWindow returns the union of the sample windows needed by every
// selector in the expression. It accounts for @ modifiers and offsets so a
// range query can fetch each selector once instead of once per step.
func selectorWindow(expr parser.Expr, lookback time.Duration, queryStart, queryEnd, at time.Time) (time.Time, time.Time) {
	var (
		start, end time.Time
		have       bool
	)
	add := func(centerStart, centerEnd time.Time, before time.Duration) {
		lo, hi := centerStart.Add(-before), centerEnd
		if !have || lo.Before(start) {
			start = lo
		}
		if !have || hi.After(end) {
			end = hi
		}
		have = true
	}
	centers := func(vs *parser.VectorSelector) (time.Time, time.Time) {
		var lo, hi time.Time
		switch vs.StartOrEnd {
		case parser.START:
			lo, hi = queryStart, queryStart
		case parser.END:
			lo, hi = queryEnd, queryEnd
		default:
			if vs.Timestamp != nil {
				lo = time.UnixMilli(*vs.Timestamp)
				hi = lo
			} else {
				lo, hi = queryStart, queryEnd
			}
		}
		return lo.Add(-vs.OriginalOffset), hi.Add(-vs.OriginalOffset)
	}
	parser.Inspect(expr, func(node parser.Node, path []parser.Node) error {
		switch n := node.(type) {
		case *parser.MatrixSelector:
			vs, ok := n.VectorSelector.(*parser.VectorSelector)
			if !ok {
				return nil
			}
			lo, hi := centers(vs)
			add(lo, hi, n.Range)
		case *parser.VectorSelector:
			if len(path) > 0 {
				if _, ok := path[len(path)-1].(*parser.MatrixSelector); ok {
					return nil
				}
			}
			lo, hi := centers(n)
			add(lo, hi, lookback)
		}
		return nil
	})
	if !have {
		return at.Add(-lookback), at
	}
	return start, end
}

func selectorFromVector(vs *parser.VectorSelector) Selector {
	return Selector{Matchers: append([]*promlabels.Matcher(nil), vs.LabelMatchers...)}
}

// --- Calls ---

func (e *evalCtx) evalCall(call *parser.Call) (Value, error) {
	v, err := e.evalCallInner(call)
	if err != nil {
		return nil, err
	}
	if vec, ok := v.(Vector); ok {
		if err := ensureUniqueVector(vec); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func (e *evalCtx) evalCallInner(call *parser.Call) (Value, error) {
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
		result, err := histogramQuantile(float64(q), vec, e.at)
		if err != nil {
			return nil, err
		}
		return result, nil
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
			stamp := s.T
			if s.SampleTValid {
				stamp = s.SampleT
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
		sort.SliceStable(v, func(i, j int) bool {
			// NaN sorts to the bottom in both directions, matching Prometheus.
			if math.IsNaN(v[i].V) {
				return false
			}
			if math.IsNaN(v[j].V) {
				return true
			}
			if name == "sort_desc" {
				return v[i].V > v[j].V
			}
			return v[i].V < v[j].V
		})
		return v, nil
	case "absent", "absent_over_time":
		return e.evalAbsent(name, call)
	case "label_replace", "label_join":
		return e.evalLabelFunction(name, call)
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
	end := e.at
	if vs, ok := ms.VectorSelector.(*parser.VectorSelector); ok {
		offset = vs.OriginalOffset
		end = e.selectorTime(vs)
	}
	end = end.Add(-offset)
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
			// Counters cannot be negative. If the counter has any slope,
			// extrapolate its zero point and do not go further left than that.
			if increase > 0 && samples[0].V >= 0 {
				durationToZero := sampledInterval * (samples[0].V / increase)
				if durationToZero < durationToStart {
					durationToStart = durationToZero
				}
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
				if v < value || math.IsNaN(value) {
					value = v
				}
			}
		case "max_over_time":
			value = values[0]
			for _, v := range values[1:] {
				if v > value || math.IsNaN(value) {
					value = v
				}
			}
		case "sum_over_time":
			sum, c := 0.0, 0.0
			for _, v := range values {
				sum, c = kahanAdd(v, sum, c)
			}
			if math.IsInf(sum, 0) {
				value = sum
			} else {
				value = sum + c
			}
		case "count_over_time":
			value = float64(len(values))
		case "last_over_time":
			value = values[len(values)-1]
		case "stddev_over_time":
			value = math.Sqrt(varianceOverTime(values))
		case "stdvar_over_time":
			value = varianceOverTime(values)
		case "present_over_time":
			value = 1
		}
		labels := cloneLabels(series.Labels)
		// last_over_time acts like an offset and keeps the metric name.
		if name != "last_over_time" {
			delete(labels, promlabels.MetricName)
		}
		out = append(out, VectorSample{Labels: labels, T: e.at.UnixMilli(), V: value})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out
}

// kahanAdd performs one Kahan-Neumaier summation step, matching the
// compensated summation used by the reference Prometheus evaluator.
func kahanAdd(inc, sum, c float64) (float64, float64) {
	t := sum + inc
	switch {
	case math.IsInf(t, 0):
		c = 0
	case math.Abs(sum) >= math.Abs(inc):
		c += (sum - t) + inc
	default:
		c += (inc - t) + sum
	}
	return t, c
}

// varianceOverTime implements the same compensated Welford update as the
// reference evaluator (population variance).
func varianceOverTime(values []float64) float64 {
	var count, mean, cMean, aux, cAux float64
	for _, v := range values {
		count++
		delta := v - (mean + cMean)
		mean, cMean = kahanAdd(delta/count, mean, cMean)
		aux, cAux = kahanAdd(delta*(v-(mean+cMean)), aux, cAux)
	}
	return (aux + cAux) / count
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
	}
	v, err := e.eval(call.Args[0])
	if err != nil {
		return nil, err
	}
	// Inverting first avoids some floating point accuracy issues and matches
	// the reference implementation, including its behaviour for a zero step.
	toNearestInverse := 1.0 / toNearest
	round := func(x float64) float64 { return math.Floor(x*toNearestInverse+0.5) / toNearestInverse }
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
	if min != nil && max != nil && *max < *min {
		switch v.(type) {
		case Scalar:
			return Scalar(math.NaN()), nil
		default:
			return Vector{}, nil
		}
	}
	clamp := func(x float64) float64 {
		if min != nil {
			x = math.Max(*min, x)
		}
		if max != nil {
			x = math.Min(*max, x)
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
	case "topk", "bottomk":
		return e.evalAggregateK(agg, op)
	case "count_values":
		return e.evalCountValues(agg)
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
	lhs, err := e.eval(bin.LHS)
	if err != nil {
		return nil, err
	}
	rhs, err := e.eval(bin.RHS)
	if err != nil {
		return nil, err
	}
	if op == "and" || op == "or" || op == "unless" {
		left, ok := lhs.(Vector)
		if !ok {
			return nil, fmt.Errorf("prom: set operator %q expects a vector on the left", op)
		}
		right, ok := rhs.(Vector)
		if !ok {
			return nil, fmt.Errorf("prom: set operator %q expects a vector on the right", op)
		}
		return e.setBinary(op, left, right, bin.VectorMatching), nil
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

func (e *evalCtx) setBinary(op string, lhs, rhs Vector, matching *parser.VectorMatching) Vector {
	if matching == nil {
		matching = &parser.VectorMatching{}
	}
	rightKeys := make(map[string]struct{}, len(rhs))
	for _, sample := range rhs {
		rightKeys[matchingKey(sample.Labels, matching)] = struct{}{}
	}
	leftKeys := make(map[string]struct{}, len(lhs))
	out := make(Vector, 0, len(lhs))
	switch op {
	case "and":
		for _, sample := range lhs {
			key := matchingKey(sample.Labels, matching)
			if _, ok := rightKeys[key]; ok {
				out = append(out, VectorSample{Labels: cloneLabels(sample.Labels), T: e.at.UnixMilli(), V: sample.V})
			}
		}
	case "unless":
		for _, sample := range lhs {
			key := matchingKey(sample.Labels, matching)
			if _, ok := rightKeys[key]; !ok {
				out = append(out, VectorSample{Labels: cloneLabels(sample.Labels), T: e.at.UnixMilli(), V: sample.V})
			}
		}
	case "or":
		for _, sample := range lhs {
			key := matchingKey(sample.Labels, matching)
			leftKeys[key] = struct{}{}
			out = append(out, VectorSample{Labels: cloneLabels(sample.Labels), T: e.at.UnixMilli(), V: sample.V})
		}
		for _, sample := range rhs {
			if _, ok := leftKeys[matchingKey(sample.Labels, matching)]; !ok {
				out = append(out, VectorSample{Labels: cloneLabels(sample.Labels), T: e.at.UnixMilli(), V: sample.V})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out
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
		// Comparisons keep the vector element's value, even when the scalar is
		// on the left-hand side.
		if isComparison(op) && !returnBool {
			v = sample.V
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
	if matching.FillValues.LHS != nil || matching.FillValues.RHS != nil {
		return nil, errors.New("prom: binary fill-in modifiers are not supported")
	}
	if matching.Card == parser.CardManyToMany {
		return nil, errors.New("prom: many-to-many matching is only valid for set operators")
	}
	// Normalize one-to-many matching by swapping sides. The left side is then
	// always the "many" side, which is where the result labels come from.
	swapped := matching.Card == parser.CardOneToMany
	if swapped {
		lhs, rhs = rhs, lhs
	}
	rightByKey := make(map[string]VectorSample, len(rhs))
	for _, sample := range rhs {
		key := matchingKey(sample.Labels, matching)
		if _, exists := rightByKey[key]; exists {
			duplicates := make([]string, 0, 2)
			for _, duplicate := range rhs {
				if matchingKey(duplicate.Labels, matching) == key {
					duplicates = append(duplicates, promlabels.FromMap(duplicate.Labels).String())
				}
			}
			sort.Strings(duplicates)
			group := promlabels.FromMap(matchingLabels(sample.Labels, matching)).String()
			return nil, fmt.Errorf("found duplicate series for the match group %s on the right hand-side of the operation: [%s];many-to-many matching not allowed: matching labels must be unique on one side", group, strings.Join(duplicates, ", "))
		}
		rightByKey[key] = sample
	}
	matched := make(map[string]struct{}, len(lhs))
	matchedOutputs := make(map[string]map[string]struct{}, len(lhs))
	out := make(Vector, 0, len(lhs))
	for _, left := range lhs {
		key := matchingKey(left.Labels, matching)
		right, ok := rightByKey[key]
		if !ok {
			continue
		}
		if matching.Card == parser.CardOneToOne {
			if _, exists := matched[key]; exists {
				return nil, errors.New("prom: multiple matches for labels: many-to-one matching must be explicit (group_left/group_right)")
			}
			matched[key] = struct{}{}
		}
		a, b := left.V, right.V
		if swapped {
			a, b = right.V, left.V
		}
		v, keep, err := binaryValue(op, a, b, returnBool)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		labels := binaryResultLabels(left.Labels, right.Labels, op, matching, returnBool)
		if matching.Card != parser.CardOneToOne {
			outputKey := labelsKey(labels)
			if matchedOutputs[key] == nil {
				matchedOutputs[key] = make(map[string]struct{})
			}
			if _, exists := matchedOutputs[key][outputKey]; exists {
				return nil, errors.New("prom: multiple matches for labels: grouping labels must ensure unique matches")
			}
			matchedOutputs[key][outputKey] = struct{}{}
		}
		out = append(out, VectorSample{Labels: labels, T: e.at.UnixMilli(), V: v})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

func binaryResultLabels(left, right map[string]string, op string, matching *parser.VectorMatching, returnBool bool) map[string]string {
	labels := cloneLabels(left)
	if isArithmetic(op) || (isComparison(op) && returnBool) {
		delete(labels, promlabels.MetricName)
	}
	if matching.Card == parser.CardOneToOne {
		if matching.On {
			keep := make(map[string]struct{}, len(matching.MatchingLabels))
			for _, name := range matching.MatchingLabels {
				keep[name] = struct{}{}
			}
			for name := range labels {
				if _, ok := keep[name]; !ok {
					delete(labels, name)
				}
			}
		} else {
			for _, name := range matching.MatchingLabels {
				delete(labels, name)
			}
		}
	}
	for _, name := range matching.Include {
		if value := right[name]; value != "" {
			labels[name] = value
		} else {
			delete(labels, name)
		}
	}
	return labels
}

func matchingKey(l map[string]string, matching *parser.VectorMatching) string {
	return labelsKey(matchingLabels(l, matching))
}

func matchingLabels(l map[string]string, matching *parser.VectorMatching) map[string]string {
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
	return selected
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
	switch op {
	case "+":
		// Unary plus is an identity operation, including the metric name.
		return v, nil
	case "-":
	default:
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
		if err := ensureUniqueVector(out); err != nil {
			return nil, err
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

// ensureUniqueVector rejects vectors with duplicate label sets, matching the
// reference engine's "vector cannot contain metrics with the same labelset"
// error. Name-dropping operations can create such collisions.
func ensureUniqueVector(vec Vector) error {
	seen := make(map[string]struct{}, len(vec))
	for _, sample := range vec {
		key := labelsKey(sample.Labels)
		if _, ok := seen[key]; ok {
			return errors.New("vector cannot contain metrics with the same labelset")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func sum(values []float64) float64 {
	var total, compensation float64
	for _, v := range values {
		total, compensation = kahanAdd(v, total, compensation)
	}
	return total + compensation
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	var (
		total        float64
		compensation float64
		mean         float64
		count        float64
		incremental  bool
	)
	for _, v := range values {
		count++
		if !incremental {
			nextTotal, nextCompensation := kahanAdd(v, total, compensation)
			if !math.IsInf(nextTotal, 0) || math.IsInf(v, 0) || math.IsInf(total, 0) {
				total, compensation = nextTotal, nextCompensation
				continue
			}
			// The direct sum would overflow. Fall back to an incremental
			// mean, matching the reference evaluator.
			incremental = true
			mean = total / (count - 1)
			compensation /= count - 1
		}
		q := (count - 1) / count
		mean, compensation = kahanAdd(v/count, q*mean, q*compensation)
	}
	if incremental {
		return mean + compensation
	}
	return (total + compensation) / count
}

func minValue(values []float64) float64 {
	out := math.NaN()
	found := false
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		if !found || v < out {
			out = v
			found = true
		}
	}
	return out
}

func maxValue(values []float64) float64 {
	out := math.NaN()
	found := false
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		if !found || v > out {
			out = v
			found = true
		}
	}
	return out
}

func variance(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	return varianceOverTime(values)
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

// histogramQuantile implements the reference Prometheus classic-histogram
// quantile algorithm, including the required +Inf bucket, bucket coalescing,
// monotonicity repair, and special handling for out-of-range quantiles.
func histogramQuantile(q float64, buckets Vector, at time.Time) (Vector, error) {
	type bucket struct {
		bound float64
		count float64
	}
	type histogram struct {
		labels  map[string]string
		buckets []bucket
		seenLE  map[float64]string
	}
	groups := make(map[string]*histogram)
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
		name := labels[promlabels.MetricName]
		delete(labels, promlabels.MetricName)
		key := labelsKey(labels)
		group := groups[key]
		if group == nil {
			group = &histogram{labels: labels, seenLE: make(map[float64]string)}
			groups[key] = group
		}
		// Two classic histograms with different metric names but the same
		// non-name labels are ambiguous. Prometheus rejects this instead of
		// silently summing their buckets.
		if previous, exists := group.seenLE[bound]; exists && previous != name {
			return nil, errors.New("vector cannot contain metrics with the same labelset")
		}
		group.seenLE[bound] = name
		group.buckets = append(group.buckets, bucket{bound: bound, count: sample.V})
	}
	out := make(Vector, 0, len(groups))
	for _, group := range groups {
		buckets := group.buckets
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].bound < buckets[j].bound })
		// Coalesce duplicate upper bounds by summing their counts.
		coalesced := buckets[:0]
		for _, current := range buckets {
			if len(coalesced) > 0 && current.bound == coalesced[len(coalesced)-1].bound {
				coalesced[len(coalesced)-1].count += current.count
				continue
			}
			coalesced = append(coalesced, current)
		}
		buckets = coalesced
		// Repair non-monotonic bucket counts the same way Prometheus does.
		prev := buckets[0].count
		for i := 1; i < len(buckets); i++ {
			current := buckets[i].count
			if current == prev {
				continue
			}
			if almostEqualBucketCounts(prev, current) {
				buckets[i].count = prev
				continue
			}
			if current < prev {
				buckets[i].count = prev
				continue
			}
			prev = current
		}

		value := math.NaN()
		switch {
		case math.IsNaN(q):
			// NaN.
		case q < 0:
			value = math.Inf(-1)
		case q > 1:
			value = math.Inf(1)
		case len(buckets) < 2 || !math.IsInf(buckets[len(buckets)-1].bound, 1):
			// A missing +Inf bucket or too few buckets cannot be interpolated.
		default:
			observations := buckets[len(buckets)-1].count
			if observations != 0 {
				rank := q * observations
				b := sort.Search(len(buckets)-1, func(i int) bool { return buckets[i].count >= rank })
				switch {
				case b == len(buckets)-1:
					value = buckets[len(buckets)-2].bound
				case b == 0 && buckets[0].bound <= 0:
					value = buckets[0].bound
				default:
					bucketStart := 0.0
					bucketEnd := buckets[b].bound
					count := buckets[b].count
					if b > 0 {
						bucketStart = buckets[b-1].bound
						count -= buckets[b-1].count
						rank -= buckets[b-1].count
					}
					value = bucketStart + (bucketEnd-bucketStart)*(rank/count)
				}
			}
		}
		out = append(out, VectorSample{Labels: group.labels, T: at.UnixMilli(), V: value})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

func almostEqualBucketCounts(a, b float64) bool {
	if a == b {
		return true
	}
	diff := math.Abs(a - b)
	sum := math.Abs(a) + math.Abs(b)
	if sum < math.SmallestNonzeroFloat64 {
		return diff < 1e-12*math.SmallestNonzeroFloat64
	}
	return diff/sum < 1e-12
}
