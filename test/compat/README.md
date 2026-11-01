# Compatibility harnesses

Black-box and differential harnesses for M3–M5:

- `prometheus/` — a separate Go module that runs the official Prometheus
  `promql` engine against the Skopos engine over 212 instant/range cases.
- `tempo/` — a Docker harness that compares the TraceQL corpus with
  `grafana/tempo:2.7.2`; run `test/compat/tempo/run.sh`.
- `loki/` — a Docker harness that compares the LogQL corpus with
  `grafana/loki:3.5.7`; run `test/compat/loki/run.sh verify`.
- `test/e2e/obi/` — a privileged Docker-level end-to-end test of real OBI eBPF
  instrumentation through Collector and ClickHouse to the gateway.

The harnesses run reference implementations as binaries or containers. They do
not link, vendor, or copy code from cerberus, oteldb, Loki, or Tempo. Tempo and
Loki are AGPL-3.0; they are only executed as reference containers.

The full Prometheus compliance tester, the complete Loki benchmark corpus, and
the three-way diff against cerberus are still beta gates. The harnesses here are
the repository's current subset.
