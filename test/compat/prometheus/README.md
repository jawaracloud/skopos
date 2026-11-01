# PromQL differential harness

This is a separate Go module. It links the official Prometheus `promql` engine
and compares it with `github.com/mrofisr/skopos/internal/prom` over one
deterministic series set. Keeping it out of the main module means the gateway's
`vendor/` tree and dependency set do not change.

## Run

```sh
cd test/compat/prometheus
go mod tidy   # only needed after changing imports
go test ./...
```

The module uses `replace github.com/mrofisr/skopos => ../../..`, so it always
tests the local worktree.

## What is compared

`harness_test.go` defines the corpus. It covers every function and operator
claimed in `docs/promql-support.md`:

- instant and range selectors, matchers, offsets, lookback and range-window
  boundaries;
- `rate`, `irate`, `increase`, counter resets, and sparse counters;
- every claimed `*_over_time` function, including NaN and quantile cases;
- `histogram_quantile`, including missing `+Inf`, non-monotonic buckets,
  out-of-range quantiles, and NaN;
- every claimed math/helper function and `sort`/`sort_desc`;
- every claimed aggregation, including `topk`, `bottomk`, and `count_values`;
- scalar/vector and vector/vector arithmetic and comparisons;
- `group_left`/`group_right` and `and`/`or`/`unless` with `on`/`ignoring`;
- `label_replace`, `label_join`, `absent`, and `absent_over_time`.

Instant results are compared by label set, exact labels and timestamps, and
values within a 1e-9 relative tolerance. Range results are compared per series,
sample-by-sample. If both engines reject a query, the case passes; if only one
rejects it, the case fails. The corpus is intentionally run at several steps,
including a 2-minute range step.

The reference storage is a small in-memory `storage.Queryable` in `storage.go`.
It avoids the TSDB and the test-only `promqltest` package while still using the
official evaluator and public storage interfaces.

## Not covered

- Samples in the future, `@` modifiers, subqueries, and negative offsets are
  outside the published alpha surface.
- Native and exponential histograms are not loaded; the exponential-histogram
  error path is covered by unit/integration tests in `internal/prom` instead.
- Ordering of `topk`/`sort` output is not compared because result ordering is
  not part of the HTTP contract; `sort` values and labels are still compared.
