# LogQL corpus

`corpus.json` is the deterministic LogQL reference corpus for M5. It contains
the input log entries and the expected normalized results for the supported
LogQL subset.

- Expected values come from a real `grafana/loki:3.5.7` instance started by
  `test/compat/loki/run.sh`. Loki's presentation-only `detected_level` label
  and `stats` object are removed.
- `internal/loki.TestLogQLCorpus` runs the same cases against the Skopos Loki
  head; it must not be used to regenerate expectations.
- To re-record from the reference implementation:

  ```sh
  test/compat/loki/run.sh record
  git diff test/corpus/logql/corpus.json
  ```

- To verify both the recorded corpus and, optionally, a seeded gateway:

  ```sh
  test/compat/loki/run.sh verify
  GATEWAY_URL=http://127.0.0.1:8080 test/compat/loki/run.sh verify
  ```

## Format

- `entries`: log lines with `timestamp` (RFC3339), `body`, and `labels`. Labels
  represent the stream labels Skopos stores in `otel_logs`.
- `cases[].kind`: `log` or `metric`.
- Log cases use `expected_logs`; only the listed timestamp and label subset are
  checked, so a case can focus on the fields it exercises.
- Metric cases use `expected` with `metric` labels and `[[unix_seconds,
  "value"], ...]` samples. Range cases have `step`; omitting `step` makes the
  harness issue an instant query.
- Range evaluation is step-aligned exactly like Loki: the first evaluation
  point is rounded down to a Unix-epoch multiple of `step`, the last is rounded
  up, and zero-count samples are omitted.

Add a case whenever support is claimed in `docs/logql-support.md`. A case that
cannot be expressed here belongs in the explicit-rejection tests instead.
