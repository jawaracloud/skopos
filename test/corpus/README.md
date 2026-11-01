# Query corpora

This directory holds the deterministic query corpora used by M3–M5.

| Corpus | Location | Reference |
| --- | --- | --- |
| TraceQL | `test/corpus/traceql/corpus.json` | Tempo 2.7.2, plus in-house expected-error cases |
| LogQL | `test/corpus/logql/corpus.json` | Loki 3.5.7 |
| PromQL | `test/compat/prometheus/` (Go corpus) | Official Prometheus `promql` engine v0.315.1 |

The TraceQL and LogQL corpora are loaded by Go tests and by the reference
container harnesses in `test/compat/`. The PromQL corpus lives in the nested
differential module so it can import the official Prometheus engine without
adding test-only dependencies to the gateway module.

Expected results come from the reference implementation. They are not generated
from the Skopos gateway. When refreshing a corpus, review the diff before
committing it.
