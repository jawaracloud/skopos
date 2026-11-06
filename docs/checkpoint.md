# Durability checkpoint

Skopos stores telemetry in ClickHouse. The durability checkpoint is a
recent-window write-ahead log that is independent of ClickHouse: if ClickHouse
commits telemetry and then loses it (PVC loss, node loss, an accidental drop),
the checkpoint can replay the original OTLP payloads back through the Collector.

It is deliberately **not** a data lake, **not** a backup, and **not** a query
source. It holds a short recent window, deletes an object after a successful
replay, and is never read by the read-only gateway.

The implementation is stdlib-only (ADR 0001). SigV4 uses `crypto/hmac` and
`crypto/sha256`, segments use `compress/gzip`, and uploads use `net/http`. No
AWS SDK, MinIO client, or compression module is linked.

## Failure model

| Failure | What protects the data |
| --- | --- |
| Checkpoint process crash or restart | `active.wal` is recovered and sealed on open; sealed segments remain until uploaded. |
| Collector or checkpoint pod restart | The checkpoint's PVC and WAL survive pod rescheduling. The Collector's file-backed queue survives a container restart; it is an `emptyDir`, so a deleted Collector pod loses queued batches. |
| ClickHouse committed data, then lost it | Objects in S3 are replayed through the Collector into ClickHouse. |
| S3 outage | Segments accumulate locally up to `MaxLocalBytes`; the receiver returns `503` with `Retry-After` instead of dropping. |
| Node loss before an upload | Segments already PUT to S3 are safe. The unsealed or unuploaded tail can be lost. A persistent volume preserves the WAL across pod rescheduling, but it does not replace the S3 PUT as the crash-consistency point. |
| Whole-cluster loss | Already-uploaded objects remain in S3. |

The acknowledgement point is the **local write**, not the S3 PUT. That keeps
ingest latency at local-disk speed. S3 is the crash-consistency point, with the
window derived below. A normal `write(2)` survives a process crash through the
page cache, but a node or power loss can discard dirty pages that have not been
`fsync`ed. `Seal` calls `fsync` before uploading, so an uploaded segment is
complete; the unsealed tail remains part of the stated loss window even when the
WAL lives on a persistent volume.

## Architecture

```text
                    ┌─────────────────────────────┐
OTel Collector ────▶│ skopos-checkpoint (serve)   │
  otlphttp/checkpoint│  OTLP/HTTP ─▶ active.wal   │
                    │                   │         │
                    │                   ▼         │
                    │          seal ─▶ seg-*.wal  │
                    │                   │ gzip    │
                    │                   ▼         │
                    └──────────────────▶ S3 ◀──────┘
                                          │
skopos-checkpoint replay ──OTLP/HTTP──▶ Collector ──▶ ClickHouse
```

The Collector fans out each signal to its ClickHouse exporter and to
`otlphttp/checkpoint`. Replay goes back through the Collector, so the
OTLP-to-ClickHouse mapping lives in exactly one place.

## Write path

1. The Collector posts one OTLP/HTTP protobuf request per signal to
   `POST /v1/traces`, `/v1/metrics`, or `/v1/logs`.
2. The receiver validates the media type (`application/x-protobuf`), accepts
   optional `Content-Encoding: gzip`, and bounds both the compressed and
   decompressed body.
3. The payload is framed with a CRC and appended to `active.wal`. `200` is
   returned after the local write.
4. The sealer seals the active segment when it reaches `SealBytes` or has been
   open for `SealInterval`, whichever comes first. It `fsync`s and renames
   `active.wal` to `seg-<unixnano>-<nonce>.wal`.
5. The uploader reads the sealed segment, gzips it, and PUTs
   `{prefix}/YYYY/MM/DD/seg-<unixnano>-<nonce>.wal.gz`. The local file is
   deleted only after the PUT succeeds. Failures retry with exponential backoff
   from 250 ms to 30 s.
6. On open, an existing `active.wal` is sealed so a torn tail can never become a
   corrupt middle record after restart.

### Record framing

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 4 | magic `0x534B4350` (`SKCP`) |
| 4 | 1 | version `1` |
| 5 | 1 | signal: `1` traces, `2` metrics, `3` logs |
| 6 | 2 | reserved |
| 8 | 4 | payload length, big-endian |
| 12 | N | OTLP protobuf payload |
| 12+N | 4 | CRC32-C (Castagnoli) of the header and payload |

A truncated final record is tolerated because a crash can tear the tail of an
append. Any other inconsistency is an explicit corruption error.

### Object layout

```text
s3://<bucket>/<prefix>/2026/11/06/seg-1793980138306430465-a1b2c3d4e5f60718.wal.gz
```

The name is the seal time in Unix nanoseconds plus a per-process random nonce,
so replicas that share an S3 prefix cannot collide. The object key is
deterministic for a segment name. Retrying after a PUT that succeeded but whose
local delete failed overwrites the same key instead of creating a duplicate
object.

## Seal policy and loss bound

Let:

- `lambda` = payload bytes arriving per second,
- `B` = segment size that triggers a seal,
- `T` = interval that triggers a seal,
- `W` = loss-window ceiling,
- `B_max` = maximum segment size that is cheap to store and upload (8 MiB),
- `M` = local byte budget (`MaxLocalBytes`).

A segment seals on whichever comes first: `B` bytes or `T` seconds. The
unshipped payload is therefore at most `max(B, lambda*T)` in the normal case.
Requiring that bound to be at most `lambda*W` gives:

```text
B <= lambda*W        and        T <= W
```

The upload cost is one PUT per sealed segment, so the request rate is `1/T`.
Minimizing requests means making `T` as large as the loss ceiling allows,
`T = W`, then setting `B = min(B_max, lambda*W)`:

```text
B* = min(B_max, lambda*W)
T* = B* / lambda <= W
PUT rate = max(1/W, lambda/B_max)
```

Examples with `B_max = 8 MiB` and `W = 15 s`:

| Arrival rate | Segment size `B*` | Seal interval `T*` | PUT rate |
| --- | --- | --- | --- |
| 64 KiB/s | 960 KiB | 15 s | 4/min |
| 1 MiB/s | 8 MiB | 8 s | 7.5/min |
| 10 MiB/s | 8 MiB | 0.8 s | 1.25/s |

The segment size is capped because one huge object makes replay less granular
and increases upload memory. When the arrival rate is low, the interval wins and
the request count stays at `1/W`.

`MaxLocalBytes` bounds an upload outage. With `M` bytes of local space, the
checkpoint absorbs for about `M/lambda` seconds, then `Append` returns
`ErrBackpressure` and the HTTP receiver returns `503`. Nothing is silently
dropped: the sender must retry. The worst-case unuploaded data is at most `M`,
and it remains recoverable while the local volume survives.

These are byte bounds, not assumptions about a smooth arrival rate. Bursty
traffic changes when a seal fires, not how much can be unshipped. Two details
tighten the practical bound: framing adds 16 bytes per record, and a single
request can exceed `B*` by up to `MaxRecordBytes` (16 MiB). A request larger
than `lambda*W` therefore makes itself the lower bound on the loss window; the
defaults assume normal OTLP batches, not a single multi-megabyte request every
few seconds.

## Configuration

Serve mode reads `SKOPOS_CHECKPOINT_*`:

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `:4319` | OTLP/HTTP listen address. |
| `DIR` | `/var/lib/skopos/checkpoint` | Local WAL directory. Use a persistent volume. |
| `SEAL_BYTES` | `8388608` | Segment size that triggers a seal; at most `209715200` (200 MiB). |
| `SEAL_INTERVAL` | `15s` | Maximum age of an open segment; at least `1ms`. |
| `MAX_LOCAL_BYTES` | `1073741824` | Local budget; appends fail with backpressure above it. |
| `MAX_REQUEST_BYTES` | `16777216` | Maximum OTLP request body (and decompressed gzip body). Must not exceed `MaxRecordBytes` (16 MiB). |
| `FLUSH_TIMEOUT` | `15s` | Bounded final upload attempt during shutdown. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `S3_ENDPOINT` | required | Scheme and host, for example `http://rustfs:9000`. |
| `S3_REGION` | `us-east-1` | SigV4 region. |
| `S3_BUCKET` | required | Existing bucket. The service does not create it. |
| `S3_PREFIX` | `skopos/checkpoint` | Root prefix for checkpoint objects. |
| `S3_ACCESS_KEY` | empty | Static access key. Empty disables signing (public bucket or test double). |
| `S3_SECRET_KEY` | empty | Static secret key. |
| `S3_SESSION_TOKEN` | empty | Optional session token. |
| `S3_VIRTUAL_HOST` | `false` | `false` uses path-style addressing (MinIO/RustFS default). |
| `S3_TIMEOUT` | `60s` | Per-request HTTP timeout. |

Replay mode adds:

| Variable | Default | Meaning |
| --- | --- | --- |
| `FORWARD_URL` | required | Collector OTLP/HTTP base URL, for example `http://otel-collector:4318`. |
| `REPLAY_DELETE_AFTER` | `false` | Delete each object after all of its records are accepted. Also available as `-delete`. |
| `REPLAY_TIMEOUT` | `60s` | Per-request timeout when posting to the Collector. |

The receiver has no authentication. Run it on the cluster network next to the
Collector, restrict access with a NetworkPolicy, and use TLS (or a service mesh)
for the S3 endpoint and the replay forward URL.

### S3 permissions

Scope a least-privilege policy to the checkpoint prefix:

- `s3:PutObject` on `<bucket>/<prefix>/*`
- `s3:GetObject` on `<bucket>/<prefix>/*`
- `s3:DeleteObject` on `<bucket>/<prefix>/*`
- `s3:ListBucket` on `<bucket>` with a `s3:prefix` condition

Static credentials are the only supported source. The access key and secret key
must be set together. IAM roles, IRSA, and IMDS are not implemented because they
would add an identity-provider dependency; use a mounted secret owned by the
workload.

## Collector wiring

```yaml
exporters:
  otlphttp/checkpoint:
    endpoint: http://skopos-checkpoint:4319
    compression: gzip
    encoding: proto
    retry_on_failure:
      enabled: true
    sending_queue:
      enabled: true
      storage: file_storage

service:
  pipelines:
    traces:
      exporters: [clickhouse, otlphttp/checkpoint]
    metrics:
      exporters: [clickhouse, otlphttp/checkpoint]
    logs:
      exporters: [clickhouse, otlphttp/checkpoint]
```

The Collector's persistent queue covers a checkpoint outage before the
checkpoint itself applies backpressure. Keep both: they fail at different
points. Monitor `otelcol_exporter_send_failed_*` for the checkpoint exporter
and the `upload_failures` field from `/stats`.

Size the checkpoint's local volume above `MaxLocalBytes` plus the filesystem
reserve. A persistent volume preserves its WAL across pod restarts and
rescheduling. If the checkpoint directory is an `emptyDir`, a pod restart loses
the unshipped window by design. The Collector's file queue is a separate
buffer; the chart uses an `emptyDir` for it and a PVC for the checkpoint.

### Helm wiring

The chart renders the exporter, the `file_storage` extension, and the queue
volume above when `checkpoint.enabled=true`. It does not create the S3 bucket.
The Collector queue is an `emptyDir`: it survives a Collector container restart
but not pod deletion. The checkpoint's PVC and WAL are the durable layer.

```yaml
checkpoint:
  enabled: true
  persistence:
    size: 2Gi
    storageClass: standard
  s3:
    endpoint: http://rustfs.observability:9000
    bucket: skopos-checkpoint
    prefix: skopos/checkpoint
    region: us-east-1
    existingSecret: skopos-checkpoint-s3
```

`checkpoint.s3.existingSecret` must contain `access-key-id` and
`secret-access-key`, and may contain `session-token`. Inline credentials are
rendered into a Secret, never into the StatefulSet. Leave both empty only for a
public bucket or a test double that accepts unsigned requests.

The checkpoint is a StatefulSet so each replica has its own `ReadWriteOnce`
WAL volume. Ingest is load-balanced by the Service, and the per-process nonce
keeps segment names unique when replicas share an S3 prefix. Size the volume
above `maxLocalBytes` plus filesystem reserve; the defaults pair a 1 GiB local
budget with a 2 GiB volume. Keep `resources.limits.memory` above `sealBytes`
plus headroom, because the uploader buffers a segment and its gzip output in
memory.

Scaling down leaves the removed replica's PVC behind, and its unuploaded tail
is not processed by another replica. Drain or accept the loss window before
scaling down.

The chart sets `terminationGracePeriodSeconds` to 40 s, which covers the 10 s
HTTP drain plus the 15 s final flush. Raise it if you raise `flushTimeout`.

### Object lifecycle

The service deletes an object after a successful `replay -delete`. Set an S3
lifecycle expiration as a backstop for objects that are never replayed. Keep it
longer than the expected recovery horizon (for example, 30 days). The chart
does not create lifecycle rules: the checkpoint policy is scoped to object
operations on its prefix, not bucket administration.

## Replay

```sh
SKOPOS_CHECKPOINT_S3_ENDPOINT=http://rustfs:9000 \
SKOPOS_CHECKPOINT_S3_BUCKET=skopos-checkpoint \
SKOPOS_CHECKPOINT_S3_PREFIX=skopos/checkpoint \
SKOPOS_CHECKPOINT_FORWARD_URL=http://otel-collector:4318 \
skopos-checkpoint replay
```

Objects are listed lexicographically (date, then segment time), decompressed,
and posted record by record as `application/x-protobuf`. Use `replay -delete`
to remove each object after every record in it receives a 2xx response. Run one
replay at a time for a given prefix.

In a chart install, run replay from a checkpoint pod so the
`SKOPOS_CHECKPOINT_*` environment is already present:

```sh
kubectl -n observability exec -it skopos-checkpoint-0 -- \
  /skopos-checkpoint replay
```

Add `-delete` to the command to remove each object after its records are
accepted.

Replay is **at-least-once** in transport. A crash between two POSTs, or after
the Collector accepts the last POST but before the S3 delete, replays some
records twice. ClickHouse makes the replayed rows idempotent:

- Traces use `ReplacingMergeTree`; the sort key ends with the OTel span
  identity `(TraceId, SpanId)`.
- Logs and metrics have no OTel record id. Their tables add a 128-bit
  materialized `_skopos_row_hash` computed from the whole row with `mapSort`
  on every attribute map, so attribute insertion order cannot split a
  duplicate.
- Every gateway read uses `FINAL`. Duplicates are hidden as soon as the POST
  returns, not only after ClickHouse's next background merge. If the gateway
  reaches a pre-migration `MergeTree` table that rejects `FINAL`, it retries
  the read without it so queries stay available; the init Job converts the
  table to `ReplacingMergeTree`.

Two byte-identical log or metric records are indistinguishable at the OTLP
level and collapse to one row. This is the only identity available without an
ingest id from the SDK. A span is collapsed only when its `(TraceId, SpanId)`
is replayed; distinct spans keep their identity.

An existing install is migrated by the ClickHouse init Job on upgrade. The Job
renames each old `MergeTree` table aside, lets `schema.sql` recreate it with the
dedup engine and key, and copies the old rows back while the new table is
already live. The Collector retries inserts during the short rename window.
Re-running the Job is safe. Large tables can take longer than the default
`clickhouse.initJob.activeDeadlineSeconds` (600 s); raise it for the upgrade. A
stale `*__skopos_pre_dedup` table means the Job was interrupted, and the next
run resumes the copy.

Any 2xx from the Collector is treated as accepted; OTLP partial-success response
bodies are not inspected.

## Operations

| Endpoint | Behavior |
| --- | --- |
| `GET /healthz` | `200 ok` while the process is alive. |
| `GET /readyz` | `200 ready` when the checkpoint is open and at least one minimum-sized record fits locally; `503` with `Retry-After` when closed or full. |
| `GET /stats` | JSON counters: appended records/bytes, sealed/uploaded segments, upload failures, active/sealed/local bytes, S3 enabled, closed. |
| `POST /v1/traces`, `/v1/metrics`, `/v1/logs` | OTLP/HTTP protobuf. `200` after the local write; `400` invalid body; `413` too large; `415` unsupported type/encoding; `503` backpressure or closed. |

On `SIGTERM`, the HTTP server stops accepting requests, the active segment is
sealed, and one bounded upload attempt runs (`FLUSH_TIMEOUT`). Segments that
cannot be uploaded stay on disk and are retried the next time the process
starts. Set `terminationGracePeriodSeconds` above `FLUSH_TIMEOUT` plus the 10 s
HTTP shutdown window; otherwise Kubernetes kills the process before the final
flush completes.

## Verification

```sh
go test ./internal/checkpoint ./cmd/skopos-checkpoint
go test -race ./internal/checkpoint
go test -run='^$' -fuzz=FuzzDecodeRecords -fuzztime=30s ./internal/checkpoint
go test -bench=. ./internal/checkpoint
```

The SigV4 tests use the AWS GET-object and PUT-object documentation vectors.
The S3 tests exercise a paginated `ListObjectsV2` double, retries, gzip
round-trips, corruption, torn tails, backpressure, local recovery, and replay.
The replay-dedup schema is exercised against a real ClickHouse by:

```sh
CLICKHOUSE_URL=http://127.0.0.1:8123 sh test/integration/dedup/run.sh
```

## Deliberate limits and follow-ups

- **No raw-event lake.** This keeps only a recent durability window. Object
  storage as a permanent source of truth is a different system (queue, worker,
  schema versioning) and is out of scope.
- **Replay deduplication is eventual at the storage layer.** `FINAL` hides
  duplicates on reads, and background merges collapse them on disk. A
  byte-identical log or metric record is treated as the same record.
- **No compactor.** ClickHouse owns compaction for its own cold tier; this
  service never rewrites ClickHouse objects.
- **No zstd.** Gzip is sufficient for a background upload and keeps the package
  dependency-free. The PUT count, not the compression ratio, dominates cost.
- **No S3 Express One Zone.** It is single-AZ, costs more, and is slower for the
  small objects this service uploads.
- **No IAM/IRSA/IMDS.** Static credentials from a mounted secret.
- **No partial-success inspection.** A 2xx means the Collector accepted the
  request; per-record partial success is surfaced by the Collector, not here.

The Collector fan-out exporter and Helm wiring are implemented. ClickHouse
replay deduplication is implemented by the `ReplacingMergeTree` schema and the
init-Job migration described above.
