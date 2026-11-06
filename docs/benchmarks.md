# Benchmarks: ClickHouse compression and gateway latency

These numbers come from a real run on the reference host, not from estimates.
The harness is in [`test/bench`](../test/bench) and can be reproduced with one
command.

Run date: 2026-11-07 (local time). This run includes the replay-dedup
`ReplacingMergeTree` schema and `FINAL` reads.

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
| `otel_logs` | 500,000 | 55,232,767 | 110.47 | 50.81 | 6.71x |
| `otel_metrics_gauge` | 576,000 | 12,970,485 | 22.52 | 22.45 | 10.12x |
| `otel_metrics_histogram` | 230,400 | 7,902,596 | 34.30 | 32.83 | 14.00x |
| `otel_metrics_sum` | 1,152,000 | 26,819,285 | 23.28 | 23.24 | 10.88x |
| `otel_traces` | 200,000 | 5,094,130 | 25.47 | 23.65 | 12.84x |
| `otel_traces_trace_id_ts` (lookup) | 40,000 | 984,616 | 24.62 | n/a | n/a |

Per-signal totals for the main signal tables:

| Signal | Rows | On disk (bytes) | Bytes/row on disk | Compressed data bytes/row |
| --- | ---: | ---: | ---: | ---: |
| Logs | 500,000 | 55,232,767 | **110.47** | **50.81** |
| Metrics | 1,958,400 | 47,692,366 | **24.35** | **24.14** |
| Traces | 200,000 | 5,094,130 | **25.47** | **23.65** |

The trace lookup table adds about `984,616 / 200,000 = 4.92` bytes per span
(one lookup row per trace, 40,000 rows for 200,000 spans).

### Interpretation

- Metrics cost about **24 bytes per sample on disk** at this cardinality. The
  replay-dedup schema adds the 16-byte `_skopos_row_hash` and a wider
  `ReplacingMergeTree` key, so this is noticeably larger than the pre-dedup
  layout.
- Logs are the expensive signal: **110.5 bytes per line on disk**, even though
  the compressed column data is only 50.8 bytes/line. The gap is the primary
  key, marks, the schema's full-text/text indexes, materialized Kubernetes
  columns, and the replay-dedup hash. Capacity planning for the default 7-day
  log TTL should use the on-disk number, not the column-data number.
- Traces cost about **25.5 bytes per span on disk** plus the trace lookup
  index. This is close to the pre-dedup layout; the hash only applies to logs
  and metrics.

## Gateway latency

Method: 3 warm-up requests, then 30 timed requests per query over loopback
HTTP/1.1. The timer includes connection setup, request, response read, and JSON
decoding. Queries run sequentially with no concurrent load. All values are
milliseconds.

| Query | p50 | p95 | p99* | min | max | Response bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `prom_instant_count` | 126.68 | 607.54 | 634.89 | 120.54 | 634.89 | 122 |
| `prom_instant_by_service` | 130.91 | 624.43 | 627.91 | 120.48 | 627.91 | 234 |
| `prom_histogram_p95` | 249.30 | 802.63 | 1138.01 | 231.83 | 1138.01 | 107 |
| `prom_range_6h` | 1149.11 | 1973.97 | 2010.29 | 512.96 | 2010.29 | 12,098 |
| `prom_series` | 212.07 | 815.25 | 838.04 | 192.72 | 838.04 | 31 |
| `prom_label_values` | 2830.85 | 3603.74 | 3617.23 | 2146.43 | 3617.23 | 280 |
| `tempo_search` | 1100.21 | 1191.56 | 1250.24 | 495.32 | 1250.24 | 20,709 |
| `tempo_trace_by_id` | 14.80 | 131.10 | 139.40 | 13.53 | 139.40 | 3,012 |
| `tempo_tags` | 1127.63 | 1228.95 | 1266.88 | 493.89 | 1266.88 | 623 |
| `loki_query_range` | 110.00 | 789.38 | 792.01 | 106.94 | 792.01 | 24,645 |

`*` With 30 samples, p99 is effectively the maximum. Treat it as such.

Exact query definitions are in [`test/bench/bench.py`](../test/bench/bench.py).
The main PromQL queries use a 5-minute rate window; the range query covers 6
hours at a 60-second step; Tempo and Loki queries cover the last hour.

### Interpretation

- Point lookups are still cheap: a trace by ID is about **15 ms p50**, but this
  run had tail spikes to **~131 ms p95** through the trace ID bloom index and a
  narrow time range. Treat the p50 as the stable figure.
- Simple Prometheus instant queries are **~127-249 ms p50** and **~608-803 ms
  p95** at this dataset size. They are slower than the pre-dedup run because
  each query probes several metric tables with `FINAL`.
- Queries that scan and aggregate a whole signal window are slower: Tempo
  search and tag discovery are **~1.1 s p50 / ~1.2 s p95**, and Loki line
  filtering is **~110 ms p50 / ~0.79 s p95**. These are alpha-path costs, not
  an SLO.
- `prom_label_values` is the outlier at **~2.8 s p50 / ~3.6 s p95**. The
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
| `prom_instant_count` | 32.86 ms | 126.68 ms |
| `prom_label_values` | 1,462.70 ms | 2,830.85 ms |
| `tempo_search` | 451.26 ms | 1,100.21 ms |
| `tempo_trace_by_id` | 9.35 ms | 14.80 ms |
| `loki_query_range` | 722.53 ms | 110.00 ms |

The PromQL and Tempo regressions are consistent with the row hash and
`FINAL` merge work. The Loki p50 improved in this run; cross-run comparisons
are directional because the host is shared with Docker and the test runner.
Storage also grew: metrics moved from ~8 to ~24 bytes/sample and logs from
~84 to ~110 bytes/line on disk.

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
10. **Tail latency is noisy.** With 30 sequential samples, several p95 values
    are much higher than p50. The run can include ClickHouse background merges
    or host contention. Use p50 for capacity comparisons and re-run before
    making SLO claims.
