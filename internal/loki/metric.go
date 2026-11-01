package loki

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// matrixSeries is one LogQL metric result series.
type matrixSeries struct {
	Labels  map[string]string
	Samples []matrixSample
}

// matrixSample is one point in a LogQL metric result.
type matrixSample struct {
	Timestamp time.Time
	Value     float64
}

// badMetricQuery marks metric query errors that are the caller's fault.
type badMetricQuery struct {
	message string
}

func (e badMetricQuery) Error() string { return e.message }

func badMetricf(format string, args ...any) error {
	return badMetricQuery{message: fmt.Sprintf(format, args...)}
}

// evaluateMetric evaluates a parsed metric query at the provided ascending
// evaluation timestamps. It fetches one extra range selector of history before
// the first step, filters entries through the selector and pipeline, groups
// them, and computes count_over_time or rate. Zero-count steps are omitted,
// matching Loki.
func (a *API) evaluateMetric(ctx context.Context, query Query, points []time.Time) ([]matrixSeries, error) {
	if query.Metric == nil {
		return nil, badMetricf("loki: not a metric query")
	}
	if len(points) == 0 {
		return nil, badMetricf("loki: no evaluation timestamps")
	}
	if a.MaxRange > 0 && query.Metric.Range > a.MaxRange {
		return nil, badMetricf("loki: range selector [%s] exceeds the maximum query range %s", query.Metric.Range, a.MaxRange)
	}
	maxSamples := a.MaxSamples
	if maxSamples <= 0 {
		maxSamples = defaultMaxSamples
	}
	if len(points) > maxSamples {
		return nil, badMetricf("loki: query has %d steps, exceeding the %d-sample limit", len(points), maxSamples)
	}
	start := points[0]
	end := points[len(points)-1]
	if end.Before(start) {
		return nil, badMetricf("loki: evaluation timestamps are not ascending")
	}

	entries, err := a.Fetcher.FetchRange(ctx, start.Add(-query.Metric.Range), end)
	if err != nil {
		return nil, err
	}

	type group struct {
		labels  map[string]string
		entries []LogEntry
	}
	groups := make(map[string]*group)
	order := make([]string, 0)
	for _, entry := range entries {
		labels, ok := query.Evaluate(entry)
		if !ok {
			continue
		}
		key, groupLabels := metricGroupKey(query, labels)
		g := groups[key]
		if g == nil {
			g = &group{labels: groupLabels}
			groups[key] = g
			order = append(order, key)
		}
		g.entries = append(g.entries, entry)
	}
	if len(groups) > 0 && len(points) > maxSamples/len(groups) {
		return nil, badMetricf("loki: query produces more than %d samples", maxSamples)
	}

	sort.Strings(order)
	result := make([]matrixSeries, 0, len(order))
	for _, key := range order {
		g := groups[key]
		sort.Slice(g.entries, func(i, j int) bool {
			return g.entries[i].Timestamp.Before(g.entries[j].Timestamp)
		})
		series := matrixSeries{Labels: g.labels, Samples: make([]matrixSample, 0, len(points))}
		for _, at := range points {
			count := countInRange(g.entries, at.Add(-query.Metric.Range), at)
			if count == 0 {
				continue
			}
			value := float64(count)
			if query.Metric.Function == "rate" {
				value = float64(count) / query.Metric.Range.Seconds()
			}
			series.Samples = append(series.Samples, matrixSample{Timestamp: at, Value: value})
		}
		if len(series.Samples) == 0 {
			continue
		}
		result = append(result, series)
	}
	return result, nil
}

func metricGroupKey(query Query, labels map[string]string) (string, map[string]string) {
	if query.Metric == nil || !query.Metric.Aggregate {
		return labelsKey(labels), cloneLabels(labels)
	}
	grouped := make(map[string]string, len(query.Metric.GroupBy))
	for _, name := range query.Metric.GroupBy {
		if value := labels[name]; value != "" {
			grouped[name] = value
		}
	}
	return labelsKey(grouped), grouped
}

func countInRange(entries []LogEntry, start, end time.Time) int {
	// Loki includes entries at both boundaries of [start, end].
	low := sort.Search(len(entries), func(i int) bool {
		return !entries[i].Timestamp.Before(start)
	})
	high := sort.Search(len(entries), func(i int) bool {
		return entries[i].Timestamp.After(end)
	})
	if high < low {
		return 0
	}
	return high - low
}
