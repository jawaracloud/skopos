# Benchmarks: ClickHouse compression, gateway latency, and concurrency

These numbers come from real runs on the reference host, not from estimates.
The harness is in [`test/bench`](../test/bench) and can be reproduced with one
command.

Run date: 2026-11-07 (local time). The sequential tables include the replay-dedup
`ReplacingMergeTree` schema and `FINAL` reads. They come from the cleaner of two
30-iteration runs; the earlier run had larger tail spikes. The 30-client
concurrency baseline comes from a separate run on the same day and host.

## Summary

| Item | Value |
| --- | --- |
| Dataset | 2,658,400 main-table rows + 40,000 trace-lookup rows |
| On disk | **103.7-103.8 MiB** total across runs (102.8-102.9 MiB main tables + 0.9-1.0 MiB lookup) |
| Storage cost | 24.35 B/metric sample, 110.22 B/log line, 25.44 B/span |
| PromQL instant | p50 134-252 ms, p95 180-283 ms |
| Tempo search | p50 592 ms, p95 701 ms |
| Tempo trace by ID | p50 17.8 ms, p95 21.1 ms |
| Loki range | p50 112.7 ms, p95 177.3 ms |
| Query compute | 68.05 ClickHouse CPU-seconds for 945 SQL queries |
| Query memory | 85.5 MiB peak per SQL query |
| Dataset generation | 24.67 CPU-seconds, 2.2 GiB peak |
| Gateway footprint (sequential) | ~0.81 core average, 1.28 core peak, 592 MiB peak |
| **30-client baseline** | **p50 5,456.64 ms, p95 40,810.87 ms, p99 63,039.76 ms, 17.81% errors, 1.88 req/s** |
| 30-client resource peak | Gateway 2.96 cores / 2.2 GiB, ClickHouse 1.54 cores / 953 MiB |

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
SKOPOS_BENCH_ITERATIONS=30 \
SKOPOS_BENCH_CONCURRENCY=30 \
SKOPOS_BENCH_CONCURRENCY_SECONDS=60 \
./test/bench/run.sh
```

Optional environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `SKOPOS_BENCH_ITERATIONS` | `30` | Timed iterations per query after 3 warm-ups. |
| `SKOPOS_BENCH_CONCURRENCY` | `30` | Concurrent mixed-workload clients. Set to `0` to skip. |
| `SKOPOS_BENCH_CONCURRENCY_SECONDS` | `60` | Target duration of the concurrent run. Set to `0` to skip. |
| `SKOPOS_BENCH_GATEWAY_PORT` | `28082` | Host port for the gateway. |
| `SKOPOS_BENCH_CLICKHOUSE_HTTP_PORT` | `18124` | Host port for ClickHouse HTTP. |
| `SKOPOS_BENCH_KEEP` | `0` | Keep the stack and volume after the run. |
| `SKOPOS_BENCH_OUTPUT` | unset | Write JSON results here; the concurrent run uses a `-concurrent.json` suffix. |

The run inserts the dataset, prints the compression tables, runs the sequential
latency loop against [`test/bench/bench.py`](../test/bench/bench.py), and then
runs the concurrent mixed workload. The script starts from a clean volume by
default.

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

## Concurrent mixed workload (30 clients)

Method: same host, dataset, and gateway build as the sequential run above. The
harness starts a fresh stack, inserts the dataset, measures compression, runs
the sequential loop, then starts 30 client threads. Each client cycles through
the same 10 queries (6 PromQL, 3 TraceQL, 1 LogQL) with a one-step offset per
client, so the workload stays mixed instead of synchronizing every client on
the same endpoint. The target duration is 60 s; the wall clock was 77.5 s
because requests already in flight when the deadline passed were allowed to
finish. Latency includes connection setup, response read, and JSON decoding.
Failed requests remain in the percentile calculation and also count toward the
error rate. Docker CPU and memory were sampled with `docker stats --no-stream`
at roughly 2-second intervals.

Command (defaults are 30 clients and 60 seconds):

```sh
SKOPOS_BENCH_KEEP=1 SKOPOS_BENCH_OUTPUT=/tmp/bench.json ./test/bench/run.sh
```

Overall result:

| Metric | Value |
| --- | ---: |
| Concurrent clients | 30 |
| Target duration | 60 s |
| Wall clock | 77.5 s |
| Requests | 146 |
| Throughput | 1.88 req/s |
| Errors | 26 (17.81%) |
| p50 | 5,456.64 ms |
| p95 | 40,810.87 ms |
| p99 | 63,039.76 ms |
| mean | 14,412.90 ms |
| On disk | 103.7 MiB (108.7 MB) |

Per-query result (latency includes failed requests):

| Query | Kind | Requests | Errors | Error rate | p50 ms | p95 ms | p99 ms | mean ms |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `prom_instant_count` | prom | 15 | 0 | 0.00% | 2,347.87 | 5,143.06 | 5,143.06 | 2,852.01 |
| `prom_instant_by_service` | prom | 18 | 0 | 0.00% | 2,348.95 | 7,479.77 | 7,479.77 | 3,693.30 |
| `prom_histogram_p95` | prom | 21 | 0 | 0.00% | 6,578.81 | 13,160.42 | 15,507.10 | 7,494.55 |
| `prom_range_6h` | prom_range | 22 | 6 | 27.27% | 24,125.12 | 40,810.87 | 40,888.77 | 25,725.43 |
| `prom_series` | prom | 19 | 0 | 0.00% | 3,768.45 | 7,366.11 | 7,366.11 | 4,062.97 |
| `prom_label_values` | prom | 21 | 12 | 57.14% | 30,847.81 | 63,039.76 | 64,273.38 | 38,848.65 |
| `tempo_search` | tempo | 3 | 3 | 100.00% | 35,438.06 | 35,799.15 | 35,799.15 | 33,774.74 |
| `tempo_trace_by_id` | tempo | 6 | 0 | 0.00% | 597.69 | 796.18 | 796.18 | 574.72 |
| `tempo_tags` | tempo | 9 | 5 | 55.56% | 34,672.19 | 39,845.25 | 39,845.25 | 29,380.67 |
| `loki_query_range` | loki | 12 | 0 | 0.00% | 726.55 | 1,273.72 | 1,273.72 | 788.51 |

Some per-query rows have only a handful of samples because slow requests
consume a client for the rest of the run; their p95/p99 values are effectively
observed maxima. Use the overall row for the contention summary.

Resource use during the concurrent run:

| Container | Samples | CPU avg (cores) | CPU peak (cores) | Memory avg | Memory peak |
| --- | ---: | ---: | ---: | ---: | ---: |
| `skopos-bench-gateway-1` | 37 | 1.61 | 2.96 | 1.4 GiB | 2.2 GiB |
| `skopos-bench-clickhouse-1` | 37 | 0.66 | 1.54 | 407 MiB | 953 MiB |

### Interpretation

- **This host is saturated at 30 clients.** Gateway CPU averages 1.61 of the 4
  available vCPUs and peaks at 2.96; ClickHouse peaks at 1.54. Combined demand
  exceeds the host's 4 threads, so queueing, not query cost, dominates the
  tail.
- **The heavy endpoints fail on the gateway's 30-second HTTP write timeout.**
  All 26 errors were `RemoteDisconnected: Remote end closed connection without
  response` on `prom_label_values`, `prom_range_6h`, `tempo_search`, and
  `tempo_tags`. Light endpoints still completed, but their p50 rose from tens
  or hundreds of milliseconds sequentially to 0.6-6.6 s.
- **Memory grows under mixed load.** The gateway averaged 1.4 GiB and peaked at
  2.2 GiB; ClickHouse peaked at 953 MiB. Together they used about 3.2 GiB of
  the host's 7.6 GiB, so memory did not hit a hard limit in this run. After the
  clients stopped, the idle gateway container dropped back to roughly 5 MiB,
  so the peak was in-flight buffering rather than a retained leak in this
  short run.
- **Storage is not the constraint.** The dataset remained at 103.7 MiB on disk;
  the bottleneck is query compute and the gateway's buffering/response path.
- **Repeatability is poor at saturation.** A second 60-second run on the same
  warm stack produced 138 requests, 43 errors (31.16%), p50 4,930.14 ms, p95
  40,696.69 ms, and p99 71,828.29 ms. Treat the numbers above as a saturation
  baseline, not an SLO or a capacity estimate for server hardware.

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
2. **Single-node, single-tenant.** Sequential and 30-client results are both
   measured on one host with no multi-tenant isolation and no replication. The
   30-client run saturates this 4-thread laptop-class host, so it is a
   contention baseline, not a capacity estimate for server hardware.
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
11. **Concurrency variance is large.** Two 60-second, 30-client runs produced
    17.81% and 31.16% error rates with p99 values of 63.0 s and 71.8 s. Once
    the host saturates, the exact mix of in-flight requests dominates. Run
    several repetitions before comparing changes.
