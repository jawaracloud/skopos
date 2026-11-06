# Benchmarks: ClickHouse compression and gateway latency

These numbers come from a real run on the reference host, not from estimates.
The harness is in [`test/bench`](../test/bench) and can be reproduced with one
command.

Run date: 2026-11-07 (local time). This run includes the replay-dedup
`ReplacingMergeTree` schema and `FINAL` reads. The latency and resource tables
below come from the cleaner of two 30-iteration runs; the earlier run had
larger tail spikes.

## Summary

| Item | Value |
| --- | --- |
| Dataset | 2,658,400 main-table rows + 40,000 trace-lookup rows |
| On disk | **103.8 MiB** total (102.9 MiB main tables + 1.0 MiB lookup) |
| Storage cost | 24.35 B/metric sample, 110.22 B/log line, 25.44 B/span |
| PromQL instant | p50 134-252 ms, p95 180-283 ms |
| Tempo search | p50 592 ms, p95 701 ms |
| Tempo trace by ID | p50 17.8 ms, p95 21.1 ms |
| Loki range | p50 112.7 ms, p95 177.3 ms |
| Query compute | 68.05 ClickHouse CPU-seconds for 945 SQL queries |
| Query memory | 85.5 MiB peak per SQL query |
| Dataset generation | 24.67 CPU-seconds, 2.2 GiB peak |
| Gateway footprint | ~0.81 core average, 1.28 core peak, 592 MiB peak |

## Host and software

| Item | Value |
| --- | --- |
| CPU | Intel Core i5-7360U, 2 cores / 4 threads, 4 vCPU |
| RAM | 7.6 GiB |
| Disk | NVMe, ext4 |
| Kernel | 6.18.55-2-cachyos-lts |
| Docker | 29.9.3 |
| Docker Compose | 5.7.0 |
| ClickHouse | `clickhouse/clickhouse-server:26.8.36.3` |
| OTel Collector contrib | not used for this benchmark (generator writes ClickHouse directly) |
| Gateway | built from this repository at `VERSION=bench` |

## Dataset

The generator in [`test/bench/generate.sql`](../test/bench/generate.sql) writes a
24-hour window ending at load time. It uses `INSERT ... SELECT FROM numbers()`
inside ClickHouse so the data is generated server-side, then runs
`OPTIMIZE TABLE ... FINAL` on every table before measuring.

| Table | Shape | Rows |
| --- | --- | ---: |
| `otel_metrics_sum` | 200 series x 5,760 samples at 15s | 1,152,000 |
| `otel_metrics_gauge` | 200 series x 2,880 samples at 30s | 576,000 |
| `otel_metrics_histogram` | 160 series x 1,440 samples at 60s, 12 buckets | 230,400 |
| `otel_logs` | 20 services, 10 message templates, 24h | 500,000 |
| `otel_traces` | 40,000 traces x 5 spans, 20 services, 24h | 200,000 |
| **Total main tables** | | **2,658,400** |

Low-cardinality resource attributes (`service.name`, `k8s.namespace.name`,
`k8s.pod.name`, `deployment.environment.name`) repeat the way they do in a small
cluster. Values and message bodies contain realistic repetition plus IDs,
numbers, trace IDs, and status codes so the compression result is not a
best-case run of identical strings.

## Commands

From the repository root:

```sh
make bench
# or
SKOPOS_BENCH_ITERATIONS=30 ./test/bench/run.sh
```

Optional environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `SKOPOS_BENCH_ITERATIONS` | `30` | Timed iterations per query after 3 warm-ups. |
| `SKOPOS_BENCH_GATEWAY_PORT` | `28082` | Host port for the gateway. |
| `SKOPOS_BENCH_CLICKHOUSE_HTTP_PORT` | `18124` | Host port for ClickHouse HTTP. |
| `SKOPOS_BENCH_KEEP` | `0` | Keep the stack and volume after the run. |
| `SKOPOS_BENCH_OUTPUT` | unset | Write the raw JSON results to this path. |

The run inserts the dataset, prints the compression tables, and then runs
[`test/bench/bench.py`](../test/bench/bench.py) against the gateway. The script
starts from a clean volume by default.

## Compression results

Measured with:

```sql
SELECT table, sum(rows), sum(bytes_on_disk)
FROM system.parts
WHERE database = 'otel' AND active
GROUP BY table;
```

and the per-column compressed/uncompressed sizes from `system.columns`.

| Table | Rows | On disk (bytes) | Bytes/row on disk | Compressed data bytes/row | Compression ratio |
| --- | ---: | ---: | ---: | ---: | ---: |
| `otel_logs` | 500,000 | 55,107,861 | 110.22 | 50.05 | 6.71x |
| `otel_metrics_gauge` | 576,000 | 12,946,963 | 22.48 | 22.10 | 10.12x |
| `otel_metrics_histogram` | 230,400 | 7,915,577 | 34.36 | 33.66 | 14.00x |
| `otel_metrics_sum` | 1,152,000 | 26,823,522 | 23.28 | 23.24 | 10.88x |
| `otel_traces` | 200,000 | 5,087,934 | 25.44 | 24.15 | 12.89x |
| `otel_traces_trace_id_ts` (lookup) | 40,000 | 982,367 | 24.56 | n/a | n/a |

Per-signal totals for the main signal tables:

| Signal | Rows | On disk (bytes) | Bytes/row on disk | Compressed data bytes/row |
| --- | ---: | ---: | ---: | ---: |
| Logs | 500,000 | 55,107,861 | **110.22** | **50.05** |
| Metrics | 1,958,400 | 47,686,062 | **24.35** | **24.13** |
| Traces | 200,000 | 5,087,934 | **25.44** | **24.15** |

The trace lookup table adds about `982,367 / 200,000 = 4.91` bytes per span
(one lookup row per trace, 40,000 rows for 200,000 spans).

### Interpretation

- Metrics cost about **24 bytes per sample on disk** at this cardinality. The
  replay-dedup schema adds the 16-byte `_skopos_row_hash` and a wider
  `ReplacingMergeTree` key, so this is noticeably larger than the pre-dedup
  layout.
- Logs are the expensive signal: **110.2 bytes per line on disk**, even though
  the compressed column data is only 50.1 bytes/line. The gap is the primary
  key, marks, the schema's full-text/text indexes, materialized Kubernetes
  columns, and the replay-dedup hash. Capacity planning for the default 7-day
  log TTL should use the on-disk number, not the column-data number.
- Traces cost about **25.4 bytes per span on disk** plus the trace lookup
  index. This is close to the pre-dedup layout; the hash only applies to logs
  and metrics.

## Gateway latency

Method: 3 warm-up requests, then 30 timed requests per query over loopback
HTTP/1.1. The timer includes connection setup, request, response read, and JSON
decoding. Queries run sequentially with no concurrent load. All values are
milliseconds.

| Query | p50 | p95 | p99* | min | max | Response bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `prom_instant_count` | 133.69 | 180.30 | 191.54 | 130.39 | 191.54 | 121 |
| `prom_instant_by_service` | 142.15 | 262.53 | 281.02 | 129.61 | 281.02 | 234 |
| `prom_histogram_p95` | 251.73 | 283.01 | 406.92 | 233.63 | 406.92 | 107 |
| `prom_range_6h` | 529.30 | 649.47 | 652.50 | 500.74 | 652.50 | 12,067 |
| `prom_series` | 203.55 | 291.22 | 357.71 | 179.46 | 357.71 | 31 |
| `prom_label_values` | 1790.06 | 2663.43 | 2809.17 | 1519.12 | 2809.17 | 280 |
| `tempo_search` | 592.07 | 701.49 | 848.53 | 537.25 | 848.53 | 20,709 |
| `tempo_trace_by_id` | 17.77 | 21.08 | 24.96 | 16.12 | 24.96 | 3,012 |
| `tempo_tags` | 606.21 | 718.77 | 735.27 | 541.44 | 735.27 | 623 |
| `loki_query_range` | 112.70 | 177.34 | 278.33 | 104.16 | 278.33 | 24,645 |

`*` With 30 samples, p99 is effectively the maximum. Treat it as such.

Exact query definitions are in [`test/bench/bench.py`](../test/bench/bench.py).
The main PromQL queries use a 5-minute rate window; the range query covers 6
hours at a 60-second step; Tempo and Loki queries cover the last hour.

### Interpretation

- Point lookups are fast: a trace by ID is about **18 ms p50 / 21 ms p95**
  through the trace ID bloom index and a narrow time range.
- Simple Prometheus instant queries are **~134-252 ms p50** and **~180-283 ms
  p95** at this dataset size. They are slower than the pre-dedup run because
  each query probes several metric tables with `FINAL`.
- Queries that scan and aggregate a whole signal window are **~0.59-0.61 s p50
  / ~0.70-0.72 s p95** for Tempo search and tags, and **~113 ms p50 / ~177 ms
  p95** for Loki line filtering. These are alpha-path costs, not an SLO.
- `prom_label_values` is the outlier at **~1.79 s p50 / ~2.66 s p95**. The
  endpoint fetches series across the gauge, sum, and histogram tables and
  deduplicates in Go; it is a candidate for pushing label discovery into
  ClickHouse before beta.
- `tempo_search` and `tempo_tags` both fetch all spans in the requested range
  and filter in Go. Their latency will grow linearly until that path is pushed
  into SQL or a trace-summary table exists.

### Before and after replay dedup

The pre-dedup run (2026-11-01) used plain `MergeTree` and no `FINAL`. The
current schema adds `ReplacingMergeTree`, `_skopos_row_hash`, and read-time
deduplication. A directional p50 comparison:

| Query | Pre-dedup (2026-11-01) | Post-dedup (2026-11-07) |
| --- | ---: | ---: |
| `prom_instant_count` | 32.86 ms | 133.69 ms |
| `prom_label_values` | 1,462.70 ms | 1,790.06 ms |
| `tempo_search` | 451.26 ms | 592.07 ms |
| `tempo_trace_by_id` | 9.35 ms | 17.77 ms |
| `loki_query_range` | 722.53 ms | 112.70 ms |

The PromQL and Tempo regressions are consistent with the row hash and
`FINAL` merge work. The Loki p50 improved in this run; cross-run comparisons
are directional because the host is shared with Docker and the test runner.
Storage also grew: metrics moved from ~8 to ~24 bytes/sample and logs from
~84 to ~110 bytes/line on disk.

## Resource use

Measured from the same 30-iteration run with ClickHouse `system.query_log` and
a 1 Hz `docker stats` sampler. Docker CPU percentages are relative to one core;
the host has 4 vCPUs.

### Dataset generation

| Metric | Value |
| --- | ---: |
| Inserts | 5 |
| ClickHouse CPU | 24.67 CPU-seconds |
| Peak query memory | 2.2 GiB |
| Rows written | 2.70M |
| Bytes written | 707 MiB |

### Query workload

300 API calls produced 945 gateway SQL queries.

| Metric | Value |
| --- | ---: |
| ClickHouse query CPU | 68.05 CPU-seconds |
| Rows read | 147.8M |
| Bytes read | ~20.8 GiB |
| Average SQL query latency | 40.89 ms |
| p95 SQL query latency | 109.8 ms |
| Peak per-query memory | 85.5 MiB |
| ClickHouse container CPU | avg 0.65 core, p95 1.17 cores, peak 2.15 cores |
| ClickHouse container memory | avg 485 MiB, sampled peak 804 MiB |
| Gateway CPU | avg 0.81 core, p95 1.26 cores, peak 1.28 cores |
| Gateway memory | avg 174 MiB, peak 592 MiB |

The ClickHouse container peak is lower than the 2.2 GiB insert peak because the
1 Hz sampler misses the shortest transient spikes. Per API call, the query path
used about **0.23 ClickHouse CPU-seconds**, read **0.49M rows / ~71 MiB**, and
peaked around **85 MiB** of query memory.

### Per signal

| Signal | SQL queries | CPU-seconds | Avg query | Rows read | Read volume | Peak memory |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Traces | 100 | 20.79 | 83.4 ms | 15.4M | 4.64 GiB | 85.5 MiB |
| Metrics histogram | 198 | 17.83 | 71.3 ms | 36.0M | 9.49 GiB | 23.1 MiB |
| Metrics sum | 198 | 16.57 | 41.6 ms | 47.9M | 2.81 GiB | 24.7 MiB |
| Metrics gauge | 198 | 7.72 | 27.0 ms | 39.0M | 1.93 GiB | 9.2 MiB |
| Logs | 19 | 4.42 | 103.5 ms | 9.36M | 2.41 GiB | 27.3 MiB |
| Metrics summary | 198 | 0.62 | 2.8 ms | 0 | 0 | 6.9 MiB |
| Metrics exponential histogram | 33 | 0.09 | 2.5 ms | 0 | 0 | 6.9 MiB |

## Limitations

Do not over-read these numbers:

1. **Reference hardware is a laptop-class 4-thread CPU.** This host has 2
   physical cores and 7.6 GiB RAM, shared with Docker and the test runner. A
   server will produce different absolute latencies.
2. **Single-node, single-tenant, sequential.** There is no concurrency, no
   multi-tenant isolation, and no replication. p95 under concurrent Grafana
   load will be worse.
3. **Warm cache.** Data was just inserted and optimized, so ClickHouse and the
   OS page cache are warm. Cold-start and post-restart latency are not measured.
4. **Synthetic data.** Compression depends on entropy. The generator repeats
   templates and resource attributes like a real cluster, but production logs
   with high-cardinality JSON or unique request IDs will compress worse. The
   full-text index cost is real and visible in the log result.
5. **Loopback client.** The client, gateway, and ClickHouse share one host. TLS,
   ingress, Grafana, and network RTT are excluded.
6. **New HTTP connection per iteration.** The Python client does not reuse a
   keep-alive connection. The overhead is small relative to these query times,
   but it is included.
7. **Alpha query coverage.** Only the listed queries are measured. Unsupported
   PromQL/LogQL/TraceQL functions are not benchmarked because they return
   explicit errors.
8. **Version-sensitive.** ClickHouse schema, exporter layout, and gateway
   behavior are pinned to the alpha revisions in this repository. Re-run the
   benchmark after any schema or query-engine change.
9. **Indexes, dedup, and external storage.** These numbers include the
   case-sensitive `idx_body` index and the replay-dedup schema
   (`ReplacingMergeTree`, `_skopos_row_hash`, and `FINAL` reads). They do not
   include tiered storage or backups; cold-tier reads have S3 latency and
   request costs. Re-run `make bench` after enabling either.
10. **Tail latency is noisy.** Two 30-iteration runs produced different p95
    values; the earlier run had spikes up to ~0.8-1.1 s on queries that show
    ~180-700 ms here. Use p50 for capacity comparisons and re-run before making
    SLO claims.
