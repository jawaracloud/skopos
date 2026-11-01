# Benchmarks: ClickHouse compression and gateway latency

These numbers come from a real run on the reference host, not from estimates.
The harness is in [`test/bench`](../test/bench) and can be reproduced with one
command.

Run date: 2026-11-01 (local time).

## Host and software

| Item | Value |
| --- | --- |
| CPU | Intel Core i5-7360U, 2 cores / 4 threads, 4 vCPU |
| RAM | 7.6 GiB |
| Disk | NVMe, ext4 |
| Kernel | 6.18.55-2-cachyos-lts |
| Docker | 29.9.2 |
| Docker Compose | 5.6.1 |
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
| `otel_logs` | 500,000 | 41,959,871 | 83.92 | 34.75 | 9.36x |
| `otel_metrics_gauge` | 576,000 | 3,362,361 | 5.84 | 5.78 | 36.52x |
| `otel_metrics_histogram` | 230,400 | 4,207,172 | 18.26 | 18.11 | 25.54x |
| `otel_metrics_sum` | 1,152,000 | 8,027,244 | 6.97 | 6.94 | 34.14x |
| `otel_traces` | 200,000 | 4,915,314 | 24.58 | 23.68 | 13.34x |
| `otel_traces_trace_id_ts` (lookup) | 40,000 | 981,782 | 24.54 | n/a | n/a |

Per-signal totals for the main signal tables:

| Signal | Rows | On disk (bytes) | Bytes/row on disk | Compressed data bytes/row |
| --- | ---: | ---: | ---: | ---: |
| Logs | 500,000 | 41,959,871 | **83.92** | **34.75** |
| Metrics | 1,958,400 | 15,596,777 | **7.96** | **7.91** |
| Traces | 200,000 | 4,915,314 | **24.58** | **23.68** |

The trace lookup table adds about `981,782 / 200,000 = 4.91` bytes per span
(one lookup row per trace, 40,000 rows for 200,000 spans).

### Interpretation

- Metrics are cheap: roughly **8 bytes per sample on disk** at this
  cardinality. Sum/gauge tables compress better than histograms because each
  histogram row also stores bucket arrays.
- Logs are the expensive signal: **83.9 bytes per line on disk**, even though
  the compressed column data is only 34.7 bytes/line. The gap is the primary
  key, marks, and the schema's full-text/text indexes plus materialized
  Kubernetes columns. Capacity planning for the default 7-day log TTL should use
  the on-disk number, not the column-data number.
- Traces cost about **24.6 bytes per span on disk** plus the trace lookup
  index. This is consistent with the OTel Collector ClickHouse exporter's
  column layout.

## Gateway latency

Method: 3 warm-up requests, then 30 timed requests per query over loopback
HTTP/1.1. The timer includes connection setup, request, response read, and JSON
decoding. Queries run sequentially with no concurrent load. All values are
milliseconds.

| Query | p50 | p95 | p99* | min | max | Response bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `prom_instant_count` | 32.86 | 40.81 | 43.15 | 30.75 | 43.15 | 123 |
| `prom_instant_by_service` | 32.33 | 40.76 | 42.74 | 31.09 | 42.74 | 236 |
| `prom_histogram_p95` | 154.91 | 206.73 | 267.80 | 150.29 | 267.80 | 107 |
| `prom_range_6h` | 438.12 | 480.82 | 485.49 | 421.56 | 485.49 | 12,289 |
| `prom_series` | 94.99 | 117.14 | 128.67 | 91.23 | 128.67 | 31 |
| `prom_label_values` | 1,462.70 | 1,548.15 | 1,558.26 | 1,419.83 | 1,558.26 | 280 |
| `tempo_search` | 451.26 | 553.99 | 613.27 | 426.03 | 613.27 | 20,709 |
| `tempo_trace_by_id` | 9.35 | 12.58 | 12.69 | 9.06 | 12.69 | 3,012 |
| `tempo_tags` | 442.88 | 524.76 | 528.57 | 429.41 | 528.57 | 623 |
| `loki_query_range` | 722.53 | 831.17 | 873.47 | 663.06 | 873.47 | 24,645 |

`*` With 30 samples, p99 is effectively the maximum. Treat it as such.

Exact query definitions are in [`test/bench/bench.py`](../test/bench/bench.py).
The main PromQL queries use a 5-minute rate window; the range query covers 6
hours at a 60-second step; Tempo and Loki queries cover the last hour.

### Interpretation

- Point lookups are fast: a trace by ID is about **9-13 ms p95** because it uses
  the trace ID bloom index and a narrow time range.
- Simple Prometheus instant queries are about **33-41 ms p95** at this dataset
  size.
- Queries that scan and aggregate a whole signal window are much slower:
  Tempo search and tag discovery are **~0.52-0.55 s p95**, and Loki line filtering is
  **~0.83 s p95**. These are alpha-path costs, not an SLO.
- `prom_label_values` is the outlier at **~1.55 s p95**. The endpoint fetches
  series across the gauge, sum, and histogram tables and deduplicates in Go; it
  is a candidate for pushing label discovery into ClickHouse before beta.
- `tempo_search` and `tempo_tags` both fetch all spans in the requested range
  and filter in Go. Their latency will grow linearly until that path is pushed
  into SQL or a trace-summary table exists.

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
