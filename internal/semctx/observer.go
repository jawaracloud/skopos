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

// ObservedLog is an observed log stream summary.
type ObservedLog struct {
	Service  string
	Severity string
	Event    string
	Count    uint64
}

// ObservedTrace is an observed trace operation summary.
type ObservedTrace struct {
	Service   string
	Operation string
	Kind      string
	Count     uint64
}

// SignalObserver reads observed log/trace summaries. Implementations should
// cache or bound the query.
type SignalObserver interface {
	ListObservedLogs(ctx context.Context) ([]ObservedLog, error)
	ListObservedTraces(ctx context.Context) ([]ObservedTrace, error)
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

// WithSignalObserver attaches observed log/trace summaries.
func (a *API) WithSignalObserver(observer SignalObserver) *API {
	a.SignalObserver = observer
	return a
}
