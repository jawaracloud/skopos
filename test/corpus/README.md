# Query corpora

This directory holds the differential query corpora used by M3–M5:

- PromQL queries compared between real Prometheus, cerberus, and the gateway.
- LogQL queries compared between real Loki and the gateway.
- TraceQL queries compared between real Tempo and the gateway.

The corpora are not populated in the M0–M2 scaffold. Each corpus entry must
come with an expected result produced by the reference implementation; results
must not be generated from the gateway itself.
