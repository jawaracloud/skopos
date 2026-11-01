# OBI end-to-end harness

This harness runs the real Skopos data path on one Linux Docker host:

```text
HTTP load -> e2e-server (no SDK)
                 |
                 | eBPF (OBI, privileged + hostPID)
                 v
           OTel Collector contrib -> ClickHouse -> Skopos gateway
                                                        |
                                          Prometheus + Tempo assertions
```

It is the first test in this repository that exercises OBI itself rather than an
OTLP generator. It asserts that OBI-produced HTTP metrics are queryable through
the gateway's Prometheus API and that OBI-produced spans are queryable through
the Tempo API (search, trace by ID, v2 protobuf, tags).

## Command

From the repository root:

```sh
make e2e-obi
# or
./test/e2e/obi/run.sh
```

To leave the stack running for inspection:

```sh
SKOPOS_E2E_KEEP=1 ./test/e2e/obi/run.sh
```

Default host ports: app `18080`, gateway `18081`, ClickHouse HTTP `18123`,
ClickHouse native `19000`. Override with `SKOPOS_E2E_APP_PORT`,
`SKOPOS_E2E_GATEWAY_PORT`, `SKOPOS_E2E_CLICKHOUSE_HTTP_PORT`, and
`SKOPOS_E2E_CLICKHOUSE_NATIVE_PORT`.

The script starts from a clean Compose project, renders the **chart's**
`deploy/helm/skopos/files/schema.sql`, builds the gateway image from the
repository, injects load, waits for OBI metrics and traces to reach ClickHouse,
runs `verify.sh`, and removes the stack and its volume on success. On failure it
keeps the containers and prints the last log lines for OBI, Collector,
ClickHouse, gateway, and the app.

You can run the assertions alone against an existing stack:

```sh
SKOPOS_E2E_GATEWAY_URL=http://127.0.0.1:18081 ./test/e2e/obi/verify.sh
```

## Requirements

- Linux kernel 5.8+ with BTF (`/sys/kernel/btf/vmlinux`) and tracefs
  (`/sys/kernel/tracing` or equivalent).
- cgroup v2 (`/sys/fs/cgroup`).
- Docker 24+ with the Compose plugin.
- Permission to run `--privileged --pid=host` containers.
- `curl`, `jq`, and `python3` (for the other harness scripts only).

The reference run for this repository used:

- CachyOS, kernel `6.18.55-2-cachyos-lts`, BTF present, tracefs mounted.
- Docker `29.9.2`, Docker Compose `5.6.1`.
- 4 vCPU, 7.6 GiB RAM.

## What the harness configures

OBI runs as the upstream `otel/ebpf-instrument:v1.0.0` image with:

- `privileged: true` (Docker then disables the default seccomp profile).
- `pid: host`.
- `/sys/kernel/tracing` and `/sys/fs/cgroup` host mounts.
- A read-only root filesystem and tmpfs for `/var/run/obi`.
- Config v2 capture policy: include only the container process that listens on
  port `8080` and whose executable matches `*/e2e-server`.
- `tracer_provider.sampler.always_on` and OTLP/gRPC exporters to the Collector.

The chart's DaemonSet uses the same privileges except that it runs with a
targeted capability set instead of full `privileged: true`. If your cluster
policy allows those capabilities but not privileged pods, the chart is the
production path; this harness intentionally uses privileged mode to test the
kernel and eBPF path without Kubernetes in the way.

## Why some settings look odd

These are deliberate fixes for real behavior observed while building the
harness, not cargo cult:

- **`tracer_provider.sampler.always_on` is required.** With OBI Config v2, a
  `tracer_provider` that has processors but no sampler falls back to a
  parent-based always-off sampler. OBI then exports HTTP metrics but zero
  traces. The chart was missing this and was fixed in
  `deploy/helm/skopos/templates/obi-configmap.yaml`.
- **The gateway listens on `:18081` inside its container.** OBI selects the
  target by open port. With `pid: host`, it can observe other containers on the
  node that listen on the same port. Keeping the gateway off `8080` avoids
  instrumenting the gateway instead of the target.
- **The harness restarts OBI after the app is healthy and waits for
  `Launching p.Tracer`.** On a host that recreates Compose containers in quick
  succession, OBI v1.0.0 sometimes logs `instrumenting process` but never
  attaches the socket iterator to the target; the first load then produces no
  telemetry. A restart after the app is listening is deterministic here.
- **OBI warns about Go 1.27.3.** OBI v1.0.0 does not recognize the Go 1.27.3
  runtime ABI (`pclntab: textStart is zero`). It falls back to generic HTTP
  instrumentation, which still produced spans and HTTP metrics in this run.
  Go-specific runtime metrics and library-level spans are not covered by this
  fallback.
- **`/sys/fs/bpf` is not mounted.** OBI logs a warning and falls back to
  process-internal maps. Traces and metrics work; features that depend on
  pinned maps (for example profile correlation) are disabled. The chart has the
  same behavior because it does not mount bpffs.
- **The chart's capability set is close but not identical to privileged mode.**
  A separate probe with the chart's `BPF`, `SYS_PTRACE`, `PERFMON`,
  `DAC_READ_SEARCH`, `NET_RAW`, `SYS_RESOURCE`, and `CHECKPOINT_RESTORE`
  capabilities plus `seccomp=unconfined` also captured HTTP spans. It logged
  `join target netns: operation not permitted` for some iterators, while the
  privileged harness does not. Treat the harness as the "known good" path; the
  chart path should be validated on each target distribution.

## Verified result

A successful run prints assertions equivalent to:

```text
verify: ok: gateway ready
verify: ok: OBI HTTP metric names are present via the Prometheus API
verify: ok: OBI request count is visible through /api/v1/query (value=96)
verify: ok: OBI request-rate range query returns 1 positive samples
verify: ok: OBI duration histogram is queryable with histogram_quantile
verify: ok: Tempo search found 20 traces for service skopos-e2e-server
verify: ok: Tempo trace-by-ID returned 3 span(s)
verify: ok: Tempo v2 protobuf trace-by-ID succeeded
verify: ok: Tempo tag search exposes resource service.name
verify: all OBI end-to-end assertions passed
```

Trace-by-ID returns generic OBI spans for the service (`GET /hello`,
`GET /work`, and internal queue/processing spans). The exact span count depends
on how many spans OBI batched into the trace; the assertion only requires at
least one.

## CI status

This harness is **manual**. GitHub-hosted `ubuntu-latest` runners may have the
required kernel, BTF, tracefs, and nested-container privileges, but that
combination is not part of GitHub's runner contract and has not been proven
stable. It is intentionally not wired into `.github/workflows/ci.yml`. Run it on
a dedicated Linux VM or a self-hosted runner with privileged Docker.

## Troubleshooting

1. Check the kernel and mounts:

   ```sh
   test -r /sys/kernel/btf/vmlinux && echo BTF ok
   mount | grep -E 'tracefs|cgroup2'
   docker info --format '{{json .SecurityOptions}} {{.CgroupVersion}}'
   ```

2. Check OBI logs for the capability and verifier errors:

   ```sh
   docker compose -p skopos-obi-e2e -f test/e2e/obi/docker-compose.yml logs obi
   ```

   - `operation not permitted` / `EPERM` on BPF syscalls usually means the
     container is not privileged and the host blocks unprivileged BPF
     (`kernel.unprivileged_bpf_disabled=2`). Use privileged mode or grant the
     documented capabilities.
   - `failed to create map` / verifier errors mention the kernel version; compare
     against OBI's support matrix.
   - `measurement error: failed to init perf ring buffer` often means `perf_event`
     is restricted; privileged mode is normally enough, but a locked-down host
     may still need `kernel.perf_event_paranoid<=1`.

3. If metrics arrive but traces do not, verify the sampler block is present in
   the OBI config (see above) and check the Collector's trace pipeline logs.

4. If the app is not discovered, confirm it listens on container port `8080`
   and check for `instrumenting process` in the OBI logs.

5. Clean up a kept stack:

   ```sh
   docker compose -p skopos-obi-e2e -f test/e2e/obi/docker-compose.yml down -v
   ```
