# ClickHouse tiered storage + backup against RustFS

This manual integration test proves the chart's tiered-storage configuration
and `BACKUP DATABASE ... TO S3(...)` path against a real S3-compatible server.
It is not part of CI because it pulls images and runs containers.

Services:

- `rustfs/rustfs:1.0.4` (digest
  `sha256:ed9210d6411d77ffceb70d94b964a3cb7d225983368bc25bebe01e995293dcc2`),
  official RustFS image; S3 API on port 9000, console on 9001.
- `clickhouse/clickhouse-server:26.8.36.3` with the same storage configuration
  shape the chart renders.
- `amazon/aws-cli:2.31.7` to create buckets and inspect objects.

Run:

```sh
PATH=/tmp/opencode/linux-amd64:$PATH ./test/integration/s3/run.sh
```

`HELM_BIN` can point at another Helm binary. `SKOPOS_S3_IT_KEEP=1` keeps the
stack for inspection.

What it checks:

1. Renders `templates/clickhouse-schema-configmap.yaml` with storage enabled and
   applies the complete schema (all signal tables plus `idx_body`).
2. Inserts an old log row and a current one, materializes TTL, and asserts the
   old part is on the `cold` S3 disk while the current part stays on `hot`.
3. Queries the cold row back through ClickHouse.
4. Runs `EXPLAIN indexes = 1` for `match(Body, ...)` and asserts `idx_body` is
   used.
5. Runs `BACKUP DATABASE otel TO S3(...)` into a bucket/prefix distinct from the
   live cold tier and lists the backup objects.
6. Runs `RESTORE DATABASE otel AS otel_restore FROM S3(...)` and compares row
   counts.

Cleanup is automatic unless kept.
