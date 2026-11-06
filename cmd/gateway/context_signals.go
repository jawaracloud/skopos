package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jawaracloud/skopos/internal/chclient"
	"github.com/jawaracloud/skopos/internal/plan"
	"github.com/jawaracloud/skopos/internal/semctx"
)

const observedSignalsTTL = 60 * time.Second

type sqlQueryer interface {
	Query(ctx context.Context, sql string, params []plan.Param) (*chclient.Result, error)
}

type observedSignals struct {
	client   sqlQueryer
	database string
	tables   plan.TableNames
	logger   *slog.Logger
	now      func() time.Time

	mu             sync.Mutex
	logsCachedAt   time.Time
	tracesCachedAt time.Time
	cachedLogs     []semctx.ObservedLog
	cachedTraces   []semctx.ObservedTrace
}

func newObservedSignals(client sqlQueryer, database string, tables plan.TableNames, logger *slog.Logger) *observedSignals {
	return &observedSignals{client: client, database: database, tables: tables, logger: logger, now: time.Now}
}

func (o *observedSignals) ListObservedLogs(ctx context.Context) ([]semctx.ObservedLog, error) {
	o.mu.Lock()
	if !o.logsCachedAt.IsZero() && o.now().Sub(o.logsCachedAt) < observedSignalsTTL {
		out := append([]semctx.ObservedLog(nil), o.cachedLogs...)
		o.mu.Unlock()
		return out, nil
	}
	o.mu.Unlock()

	query := fmt.Sprintf(
		"SELECT ServiceName, SeverityText, EventName, count() AS count FROM `%s`.`%s` FINAL "+
			"WHERE Timestamp >= now() - INTERVAL 7 DAY "+
			"GROUP BY ServiceName, SeverityText, EventName ORDER BY count DESC LIMIT 200",
		o.database, o.tables.Logs,
	)
	result, err := o.client.Query(ctx, query, nil)
	if err != nil {
		o.logger.Warn("observed log metadata unavailable", "error", err)
		return nil, err
	}
	out := make([]semctx.ObservedLog, 0, result.Len())
	for i := 0; i < result.Len(); i++ {
		count, _ := result.Int64(i, "count")
		out = append(out, semctx.ObservedLog{
			Service:  resultString(result, i, "ServiceName"),
			Severity: resultString(result, i, "SeverityText"),
			Event:    resultString(result, i, "EventName"),
			Count:    uint64(maxInt64(count, 0)),
		})
	}
	o.mu.Lock()
	o.cachedLogs = append([]semctx.ObservedLog(nil), out...)
	o.logsCachedAt = o.now()
	o.mu.Unlock()
	return out, nil
}

func (o *observedSignals) ListObservedTraces(ctx context.Context) ([]semctx.ObservedTrace, error) {
	o.mu.Lock()
	if !o.tracesCachedAt.IsZero() && o.now().Sub(o.tracesCachedAt) < observedSignalsTTL {
		out := append([]semctx.ObservedTrace(nil), o.cachedTraces...)
		o.mu.Unlock()
		return out, nil
	}
	o.mu.Unlock()

	query := fmt.Sprintf(
		"SELECT ServiceName, SpanName, SpanKind, count() AS count FROM `%s`.`%s` FINAL "+
			"WHERE Timestamp >= now() - INTERVAL 7 DAY "+
			"GROUP BY ServiceName, SpanName, SpanKind ORDER BY count DESC LIMIT 200",
		o.database, o.tables.Traces,
	)
	result, err := o.client.Query(ctx, query, nil)
	if err != nil {
		o.logger.Warn("observed trace metadata unavailable", "error", err)
		return nil, err
	}
	out := make([]semctx.ObservedTrace, 0, result.Len())
	for i := 0; i < result.Len(); i++ {
		count, _ := result.Int64(i, "count")
		out = append(out, semctx.ObservedTrace{
			Service:   resultString(result, i, "ServiceName"),
			Operation: resultString(result, i, "SpanName"),
			Kind:      resultString(result, i, "SpanKind"),
			Count:     uint64(maxInt64(count, 0)),
		})
	}
	o.mu.Lock()
	o.cachedTraces = append([]semctx.ObservedTrace(nil), out...)
	o.tracesCachedAt = o.now()
	o.mu.Unlock()
	return out, nil
}

func resultString(result *chclient.Result, row int, column string) string {
	value, _ := result.String(row, column)
	return value
}

func maxInt64(value, fallback int64) int64 {
	if value < fallback {
		return fallback
	}
	return value
}
