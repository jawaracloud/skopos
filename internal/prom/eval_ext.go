package prom

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// evalAggregateK implements topk and bottomk. Both return the original series
// (labels and metric name intact), not the aggregation grouping labels.
func (e *evalCtx) evalAggregateK(agg *parser.AggregateExpr, op string) (Value, error) {
	v, err := e.eval(agg.Expr)
	if err != nil {
		return nil, err
	}
	vec, ok := v.(Vector)
	if !ok {
		return nil, fmt.Errorf("prom: %s expects a vector", op)
	}
	if agg.Param == nil {
		return nil, fmt.Errorf("prom: %s requires a scalar parameter", op)
	}
	kParam, err := e.evalScalar(agg.Param)
	if err != nil {
		return nil, err
	}
	kf := float64(kParam)
	if math.IsNaN(kf) {
		return nil, errors.New("Parameter value is NaN")
	}
	k := int64(kf)
	switch {
	case kf < 0:
		k = 0
	case math.IsInf(kf, 1):
		k = int64(len(vec))
	case kf > float64(len(vec)):
		k = int64(len(vec))
	}

	type candidate struct {
		labels map[string]string
		value  float64
	}
	groups := make(map[string][]candidate)
	for _, sample := range vec {
		labels := aggregateLabels(sample.Labels, agg.Grouping, agg.Without)
		key := labelsKey(labels)
		groups[key] = append(groups[key], candidate{labels: cloneLabels(sample.Labels), value: sample.V})
	}

	out := make(Vector, 0, len(vec))
	for _, candidates := range groups {
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := candidates[i].value, candidates[j].value
			// NaN always sorts last, matching the reference topk/bottomk heaps.
			if math.IsNaN(a) {
				return false
			}
			if math.IsNaN(b) {
				return true
			}
			if op == "bottomk" {
				return a < b
			}
			return a > b
		})
		if int64(len(candidates)) > k {
			candidates = candidates[:k]
		}
		for _, item := range candidates {
			out = append(out, VectorSample{Labels: item.labels, T: e.at.UnixMilli(), V: item.value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

// evalCountValues implements count_values(label, vector).
func (e *evalCtx) evalCountValues(agg *parser.AggregateExpr) (Value, error) {
	valueLabel, err := callStringArg(agg.Param, 0)
	if err != nil {
		return nil, fmt.Errorf("prom: count_values: %w", err)
	}
	if !validLegacyLabelName(valueLabel) {
		return nil, fmt.Errorf("prom: count_values: invalid label name \"%s\"", valueLabel)
	}
	v, err := e.eval(agg.Expr)
	if err != nil {
		return nil, err
	}
	vec, ok := v.(Vector)
	if !ok {
		return nil, fmt.Errorf("prom: count_values expects a vector")
	}
	grouping := append([]string(nil), agg.Grouping...)
	if !agg.Without {
		grouping = append(grouping, valueLabel)
	}

	type group struct {
		labels map[string]string
		count  float64
	}
	groups := make(map[string]*group)
	for _, sample := range vec {
		labels := cloneLabels(sample.Labels)
		labels[valueLabel] = strconv.FormatFloat(sample.V, 'f', -1, 64)
		groupLabels := aggregateLabels(labels, grouping, agg.Without)
		key := labelsKey(groupLabels)
		entry := groups[key]
		if entry == nil {
			entry = &group{labels: groupLabels}
			groups[key] = entry
		}
		entry.count++
	}
	out := make(Vector, 0, len(groups))
	for _, entry := range groups {
		out = append(out, VectorSample{Labels: entry.labels, T: e.at.UnixMilli(), V: entry.count})
	}
	sort.Slice(out, func(i, j int) bool { return labelsKey(out[i].Labels) < labelsKey(out[j].Labels) })
	return out, nil
}

// evalAbsent implements absent and absent_over_time. Both return an empty
// vector when their argument produces data, and 1 with the equality matchers of
// a direct selector when it does not.
func (e *evalCtx) evalAbsent(name string, call *parser.Call) (Value, error) {
	if len(call.Args) != 1 {
		return nil, fmt.Errorf("prom: %s expects one argument", name)
	}
	var empty bool
	if name == "absent_over_time" {
		matrix, err := e.evalMatrixArg(call, 0)
		if err != nil {
			return nil, err
		}
		empty = len(matrix) == 0
	} else {
		vec, err := e.evalVectorArg(call, 0)
		if err != nil {
			return nil, err
		}
		empty = len(vec) == 0
	}
	if !empty {
		return Vector{}, nil
	}
	return Vector{{Labels: absentLabels(call.Args[0]), T: e.at.UnixMilli(), V: 1}}, nil
}

func absentLabels(expr parser.Expr) map[string]string {
	for {
		paren, ok := expr.(*parser.ParenExpr)
		if !ok {
			break
		}
		expr = paren.Expr
	}
	var matchers []*promlabels.Matcher
	switch node := expr.(type) {
	case *parser.VectorSelector:
		matchers = node.LabelMatchers
	case *parser.MatrixSelector:
		selector, ok := node.VectorSelector.(*parser.VectorSelector)
		if !ok {
			return map[string]string{}
		}
		matchers = selector.LabelMatchers
	default:
		return map[string]string{}
	}
	out := make(map[string]string)
	has := make(map[string]bool)
	for _, matcher := range matchers {
		if matcher.Name == promlabels.MetricName {
			continue
		}
		if matcher.Type == promlabels.MatchEqual && !has[matcher.Name] {
			out[matcher.Name] = matcher.Value
			has[matcher.Name] = true
			continue
		}
		delete(out, matcher.Name)
	}
	return out
}

// evalLabelFunction implements label_replace and label_join.
func (e *evalCtx) evalLabelFunction(name string, call *parser.Call) (Value, error) {
	vec, err := e.evalVectorArg(call, 0)
	if err != nil {
		return nil, err
	}
	var transform func(map[string]string)
	switch name {
	case "label_replace":
		if len(call.Args) != 5 {
			return nil, fmt.Errorf("prom: label_replace expects five arguments")
		}
		dst, err := callStringArg(call.Args[1], 1)
		if err != nil {
			return nil, err
		}
		repl, err := callStringArg(call.Args[2], 2)
		if err != nil {
			return nil, err
		}
		src, err := callStringArg(call.Args[3], 3)
		if err != nil {
			return nil, err
		}
		regexStr, err := callStringArg(call.Args[4], 4)
		if err != nil {
			return nil, err
		}
		if !validLegacyLabelName(dst) {
			return nil, fmt.Errorf("prom: label_replace: invalid destination label name %q", dst)
		}
		re, err := regexp.Compile("^(?s:" + regexStr + ")$")
		if err != nil {
			return nil, fmt.Errorf("prom: label_replace: invalid regular expression: %w", err)
		}
		transform = func(labels map[string]string) {
			srcValue := labels[src]
			indexes := re.FindStringSubmatchIndex(srcValue)
			if indexes == nil {
				return
			}
			value := string(re.ExpandString(nil, repl, srcValue, indexes))
			if value == "" {
				delete(labels, dst)
				return
			}
			labels[dst] = value
		}
	case "label_join":
		if len(call.Args) < 3 {
			return nil, fmt.Errorf("prom: label_join expects at least three arguments")
		}
		dst, err := callStringArg(call.Args[1], 1)
		if err != nil {
			return nil, err
		}
		separator, err := callStringArg(call.Args[2], 2)
		if err != nil {
			return nil, err
		}
		sources := make([]string, 0, len(call.Args)-3)
		for i := 3; i < len(call.Args); i++ {
			source, err := callStringArg(call.Args[i], i)
			if err != nil {
				return nil, err
			}
			if !validLegacyLabelName(source) && source != promlabels.MetricName {
				return nil, fmt.Errorf("prom: label_join: invalid source label name %q", source)
			}
			sources = append(sources, source)
		}
		if !validLegacyLabelName(dst) {
			return nil, fmt.Errorf("prom: label_join: invalid destination label name %q", dst)
		}
		transform = func(labels map[string]string) {
			values := make([]string, len(sources))
			for i, source := range sources {
				values[i] = labels[source]
			}
			value := strings.Join(values, separator)
			if value == "" {
				delete(labels, dst)
				return
			}
			labels[dst] = value
		}
	default:
		return nil, fmt.Errorf("prom: unsupported label function %q", name)
	}

	out := make(Vector, 0, len(vec))
	seen := make(map[string]struct{}, len(vec))
	for _, sample := range vec {
		labels := cloneLabels(sample.Labels)
		transform(labels)
		key := labelsKey(labels)
		if _, exists := seen[key]; exists {
			return nil, errors.New("vector cannot contain metrics with the same labelset")
		}
		seen[key] = struct{}{}
		out = append(out, VectorSample{Labels: labels, T: sample.T, V: sample.V})
	}
	return out, nil
}

func callStringArg(expr parser.Expr, index int) (string, error) {
	if expr == nil {
		return "", fmt.Errorf("argument %d is missing", index+1)
	}
	for {
		paren, ok := expr.(*parser.ParenExpr)
		if !ok {
			break
		}
		expr = paren.Expr
	}
	literal, ok := expr.(*parser.StringLiteral)
	if !ok {
		return "", fmt.Errorf("argument %d must be a string", index+1)
	}
	return literal.Val, nil
}

func validLegacyLabelName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9')
		if !valid {
			return false
		}
	}
	return true
}
