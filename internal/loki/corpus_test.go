package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

type corpusFile struct {
	Description string        `json:"description"`
	Entries     []corpusEntry `json:"entries"`
	Cases       []corpusCase  `json:"cases"`
}

type corpusEntry struct {
	Timestamp string            `json:"timestamp"`
	Body      string            `json:"body"`
	Labels    map[string]string `json:"labels"`
}

type corpusCase struct {
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	Query        string         `json:"query"`
	Start        string         `json:"start"`
	End          string         `json:"end"`
	Step         string         `json:"step"`
	ExpectedLogs []corpusLog    `json:"expected_logs"`
	Expected     []corpusMetric `json:"expected"`
}

type corpusLog struct {
	Timestamp string            `json:"timestamp"`
	Labels    map[string]string `json:"labels"`
}

type corpusMetric struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}

type rangeFetcher struct {
	entries []LogEntry
}

func (f rangeFetcher) FetchRange(_ context.Context, start, end time.Time) ([]LogEntry, error) {
	out := make([]LogEntry, 0, len(f.entries))
	for _, entry := range f.entries {
		if entry.Timestamp.Before(start) || entry.Timestamp.After(end) {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func TestLogQLCorpus(t *testing.T) {
	data, err := os.ReadFile("../../test/corpus/logql/corpus.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var corpus corpusFile
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(corpus.Entries) == 0 || len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}
	entries := make([]LogEntry, 0, len(corpus.Entries))
	for _, raw := range corpus.Entries {
		timestamp, err := time.Parse(time.RFC3339Nano, raw.Timestamp)
		if err != nil {
			t.Fatalf("entry timestamp %q: %v", raw.Timestamp, err)
		}
		entries = append(entries, LogEntry{Timestamp: timestamp, Body: raw.Body, Labels: raw.Labels})
	}

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			query, err := Parse(tc.Query)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			start := parseCorpusTime(t, tc.Start)
			end := parseCorpusTime(t, tc.End)
			switch tc.Kind {
			case "log":
				checkLogCorpusCase(t, query, entries, start, end, tc.ExpectedLogs)
			case "metric":
				checkMetricCorpusCase(t, query, entries, start, end, tc.Step, tc.Expected)
			default:
				t.Fatalf("unknown corpus case kind %q", tc.Kind)
			}
		})
	}
}

func checkLogCorpusCase(t *testing.T, query Query, entries []LogEntry, start, end time.Time, expected []corpusLog) {
	t.Helper()
	got := make([]LogEntry, 0)
	for _, entry := range entries {
		if entry.Timestamp.Before(start) || entry.Timestamp.After(end) {
			continue
		}
		labels, ok := query.Evaluate(entry)
		if !ok {
			continue
		}
		entry.Labels = labels
		got = append(got, entry)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Timestamp.Before(got[j].Timestamp) })
	if len(got) != len(expected) {
		t.Fatalf("matched %d log lines, want %d: %#v", len(got), len(expected), got)
	}
	for i, want := range expected {
		timestamp := parseCorpusTime(t, want.Timestamp)
		if !got[i].Timestamp.Equal(timestamp) {
			t.Fatalf("line %d timestamp = %s, want %s", i, got[i].Timestamp, timestamp)
		}
		for name, value := range want.Labels {
			if got[i].Labels[name] != value {
				t.Fatalf("line %d label %q = %q, want %q (labels %#v)", i, name, got[i].Labels[name], value, got[i].Labels)
			}
		}
	}
}

func checkMetricCorpusCase(t *testing.T, query Query, entries []LogEntry, start, end time.Time, stepText string, expected []corpusMetric) {
	t.Helper()
	if query.Metric == nil {
		t.Fatal("metric corpus case parsed as a log query")
	}
	step, err := parseRangeDuration(stepText)
	if err != nil {
		t.Fatalf("step %q: %v", stepText, err)
	}
	points, err := evaluationPoints(start, end, step, defaultMaxSamples)
	if err != nil {
		t.Fatalf("evaluation points: %v", err)
	}
	api := NewAPI(rangeFetcher{entries: entries}, "corpus")
	series, err := api.evaluateMetric(context.Background(), query, points)
	if err != nil {
		t.Fatalf("evaluateMetric: %v", err)
	}
	got := make(map[string]matrixSeries, len(series))
	for _, s := range series {
		got[labelsKey(s.Labels)] = s
	}
	if len(got) != len(expected) {
		t.Fatalf("got %d series, want %d: %#v", len(got), len(expected), series)
	}
	for _, want := range expected {
		key := labelsKey(want.Metric)
		series, ok := got[key]
		if !ok {
			t.Fatalf("missing series %#v (got %#v)", want.Metric, series)
		}
		if len(series.Samples) != len(want.Values) {
			t.Fatalf("series %#v has %d samples, want %d: %#v", want.Metric, len(series.Samples), len(want.Values), series.Samples)
		}
		for i, pair := range want.Values {
			if len(pair) != 2 {
				t.Fatalf("expected value has %d elements", len(pair))
			}
			timestamp, err := jsonNumberFloat(pair[0])
			if err != nil {
				t.Fatalf("expected timestamp: %v", err)
			}
			value, ok := pair[1].(string)
			if !ok {
				t.Fatalf("expected value is not a string: %#v", pair[1])
			}
			gotTimestamp := float64(series.Samples[i].Timestamp.UnixNano()) / 1e9
			if gotTimestamp != timestamp || formatMetricFloat(series.Samples[i].Value) != value {
				t.Fatalf("series %#v sample %d = (%v, %q), want (%v, %q)", want.Metric, i, gotTimestamp, formatMetricFloat(series.Samples[i].Value), timestamp, value)
			}
		}
	}
}

func parseCorpusTime(t *testing.T, value string) time.Time {
	t.Helper()
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("timestamp %q: %v", value, err)
	}
	return timestamp
}

func jsonNumberFloat(value any) (float64, error) {
	number, ok := value.(json.Number)
	if !ok {
		if f, ok := value.(float64); ok {
			return f, nil
		}
		return 0, fmt.Errorf("not a JSON number: %#v", value)
	}
	return number.Float64()
}
