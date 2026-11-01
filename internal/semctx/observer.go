package semctx

import "context"

// ObservedMetric is factual metadata read from the telemetry store. It is not
// a semantic context entry: type/unit/description are observed values, and the
// API labels them with a lower-confidence source.
type ObservedMetric struct {
	Name        string
	Type        string
	Unit        string
	Description string
}

// Observer reads observed metric metadata. Implementations should cache or
// bound this work; it is called only for unknown metrics or explicit coverage
// requests.
type Observer interface {
	Observe(ctx context.Context, name string) (ObservedMetric, bool, error)
	ListObserved(ctx context.Context) ([]ObservedMetric, error)
}

// WithObserver attaches an observed-metadata provider.
func (a *API) WithObserver(observer Observer) *API {
	a.Observer = observer
	return a
}
