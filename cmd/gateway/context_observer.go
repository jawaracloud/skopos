package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jawaracloud/skopos/internal/prom"
	"github.com/jawaracloud/skopos/internal/semctx"
)

const (
	observedMetadataTTL     = 30 * time.Second
	observedMetadataTimeout = 2 * time.Second
)

// metadataFetcher is the subset of prom.ClickHouseFetcher used by the context
// observer. Keeping it small makes the cache testable without ClickHouse.
type metadataFetcher interface {
	Metadata(ctx context.Context, metric string) (map[string]prom.Metadata, error)
}

type contextObserver struct {
	fetcher metadataFetcher
	logger  *slog.Logger
	now     func() time.Time

	mu       sync.Mutex
	cachedAt time.Time
	cached   []semctx.ObservedMetric
}

func newContextObserver(fetcher metadataFetcher, logger *slog.Logger) *contextObserver {
	return &contextObserver{fetcher: fetcher, logger: logger, now: time.Now}
}

func (o *contextObserver) Observe(ctx context.Context, name string) (semctx.ObservedMetric, bool, error) {
	items, err := o.list(ctx)
	if err != nil {
		return semctx.ObservedMetric{}, false, err
	}
	want := prom.NormalizeLabelName(name)
	for _, item := range items {
		if item.Name == want {
			return item, true, nil
		}
	}
	return semctx.ObservedMetric{}, false, nil
}

func (o *contextObserver) ListObserved(ctx context.Context) ([]semctx.ObservedMetric, error) {
	return o.list(ctx)
}

func (o *contextObserver) list(ctx context.Context) ([]semctx.ObservedMetric, error) {
	o.mu.Lock()
	if !o.cachedAt.IsZero() && o.now().Sub(o.cachedAt) < observedMetadataTTL {
		cached := append([]semctx.ObservedMetric(nil), o.cached...)
		o.mu.Unlock()
		return cached, nil
	}
	o.mu.Unlock()

	queryCtx, cancel := context.WithTimeout(ctx, observedMetadataTimeout)
	defer cancel()
	metadata, err := o.fetcher.Metadata(queryCtx, "")
	if err != nil {
		o.logger.Warn("observed context metadata unavailable", "error", err)
		return nil, err
	}
	items := make([]semctx.ObservedMetric, 0, len(metadata))
	for name, item := range metadata {
		items = append(items, semctx.ObservedMetric{
			Name: name, Type: item.Type, Unit: item.Unit, Description: item.Help,
		})
	}
	o.mu.Lock()
	o.cached = append([]semctx.ObservedMetric(nil), items...)
	o.cachedAt = o.now()
	o.mu.Unlock()
	return items, nil
}
