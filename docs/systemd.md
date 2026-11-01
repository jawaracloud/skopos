# Running Skopos as a systemd service

The Skopos gateway is a stateless Go binary. It can run directly under systemd
on a Linux host without Kubernetes, Docker, or a container runtime. ClickHouse
stays a separate service, and the OpenTelemetry Collector and OBI remain
separate agents.

This guide covers the **gateway**. It does not turn OBI into an unprivileged
service and it does not bundle ClickHouse.

## What you get

| Benefit | Detail |
| --- | --- |
| Small runtime | One static binary, no container runtime or Kubernetes control plane. |
| Native supervision | `Restart=on-failure`, start rate limiting, ordered startup, and clean `SIGTERM` shutdown. |
| Journald logs | Structured JSON request/startup logs in `journalctl`, with no log file rotation to manage. |
| Sandboxing | `NoNewPrivileges`, empty capability sets, `ProtectSystem=strict`, `ProtectHome`, private tmp/devices, and restricted address families. |
| Simple upgrades | Replace the binary and `systemctl restart`; roll back by restoring the previous binary. |
| Direct networking | Bind to loopback and put Caddy/Nginx/Traefik in front for TLS and auth if needed. |
| Split read path | Run `skopos-query` internally and `skopos-frontend` publicly on the same host. |
| Works with existing data plane | Point at an existing ClickHouse and Collector; no migration of the storage layer. |

What you do **not** get:

- No Helm init job, so schema creation/migration is manual.
- No Kubernetes NetworkPolicy, HPA, or PodDisruptionBudget.
- No ClickHouse replication/HA; that remains a ClickHouse concern.
- No automatic OBI privilege handling. OBI needs eBPF, `hostPID`, tracefs/cgroup
  mounts, and capabilities; container/systemd deployment requires deliberate
  host configuration.
- No per-user authorization. Basic auth is one shared credential.

## Files

```text
deploy/systemd/
  skopos-gateway.service       single process: API + query heads
  skopos-query.service         internal query worker (distributed mode)
  skopos-frontend.service      public proxy (distributed mode)
  gateway.env.example
  query.env.example
  frontend.env.example
  render-schema.sh             local-only ClickHouse DDL renderer
```

## Prerequisites

- Linux with systemd 249 or newer (for the hardening settings used here).
- A running ClickHouse 26.8.x with the OTel exporter schema.
- A read-only ClickHouse user for the gateway.
- OTel Collector and OBI configured separately if you need ingestion.

## 1. Build and install the binary

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always)" \
  -o /tmp/skopos-gateway ./cmd/gateway

sudo install -m 0755 /tmp/skopos-gateway /usr/local/bin/skopos-gateway
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin skopos || true
sudo install -d -m 0750 -o root -g skopos /etc/skopos
```

## 2. Create the ClickHouse schema and user

Render the chart's local-only schema:

```sh
./deploy/systemd/render-schema.sh /tmp/skopos-schema.sql
clickhouse-client --multiquery < /tmp/skopos-schema.sql
```

If you use tiered S3 storage, use the Helm/S3 path in
[storage.md](storage.md) instead of this local-only renderer.

Create a read-only gateway user. Replace the password; avoid leaving it in
shell history by using `clickhouse-client` interactively or a protected file.

```sql
CREATE USER IF NOT EXISTS skopos_gateway
  IDENTIFIED WITH sha256_password BY 'change-me'
  SETTINGS readonly = 2,
           max_execution_time = 30,
           max_memory_usage = 1073741824,
           max_rows_to_read = 10000000,
           max_result_rows = 100000;
GRANT SELECT ON otel.* TO skopos_gateway;
```

## 3. Configure the service

Single-process mode:

```sh
sudo cp deploy/systemd/gateway.env.example /etc/skopos/gateway.env
sudo editor /etc/skopos/gateway.env
sudo chown root:skopos /etc/skopos/gateway.env
sudo chmod 0640 /etc/skopos/gateway.env
```

At minimum set:

```sh
SKOPOS_LISTEN_ADDR=127.0.0.1:8080
SKOPOS_CLICKHOUSE_URL=http://127.0.0.1:8123
SKOPOS_CLICKHOUSE_DATABASE=otel
SKOPOS_CLICKHOUSE_USER=skopos_gateway
SKOPOS_CLICKHOUSE_PASSWORD=change-me
```

Keep the listener on loopback unless a firewall and TLS/auth front end protect
it. To enable Basic auth:

```sh
SKOPOS_AUTH_ENABLED=true
SKOPOS_AUTH_USERNAME=viewer
SKOPOS_AUTH_PASSWORD=change-me
```

`/healthz` and `/readyz` stay unauthenticated for probes. All query routes and
`/version` require the credential.

## 4. Install the unit

Single process:

```sh
sudo install -m 0644 deploy/systemd/skopos-gateway.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now skopos-gateway
sudo systemctl status skopos-gateway
```

Check:

```sh
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/readyz
curl -u viewer:change-me -s http://127.0.0.1:8080/version
```

Logs:

```sh
journalctl -u skopos-gateway -f
```

## 5. Optional distributed mode

Use two units on the same host (or one host for the frontend and a private
network address for query workers):

```sh
sudo cp deploy/systemd/query.env.example /etc/skopos/query.env
sudo cp deploy/systemd/frontend.env.example /etc/skopos/frontend.env
sudo editor /etc/skopos/query.env /etc/skopos/frontend.env
sudo chown root:skopos /etc/skopos/query.env /etc/skopos/frontend.env
sudo chmod 0640 /etc/skopos/query.env /etc/skopos/frontend.env

sudo install -m 0644 \
  deploy/systemd/skopos-query.service \
  deploy/systemd/skopos-frontend.service \
  /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now skopos-query skopos-frontend
```

The frontend listens on `127.0.0.1:8080` and proxies to
`http://127.0.0.1:8081`. If auth is enabled, use the same credentials in both
env files: the frontend validates the request and forwards the Authorization
header; the query worker validates it again.

For multiple query workers, set:

```sh
SKOPOS_QUERY_URLS=http://127.0.0.1:8081,http://127.0.0.1:8082
```

and run `skopos-query@.service` instances or separate host units.

## 6. TLS and reverse proxy

Do not expose Basic auth over plain HTTP. Put Caddy, Nginx, or Traefik in front:

```text
Grafana -> https://skopos.example.com -> 127.0.0.1:8080
```

The reverse proxy terminates TLS and forwards to the gateway. If you use the
chart's Basic auth, configure Grafana's datasources with the same credentials.
If you use proxy-level SSO instead, leave `SKOPOS_AUTH_ENABLED=false` and let the
proxy authenticate.

## Operations

Upgrade:

```sh
sudo install -m 0755 ./skopos-gateway /usr/local/bin/skopos-gateway
sudo systemctl restart skopos-gateway
```

Rollback: restore the previous binary and restart.

Stop and disable:

```sh
sudo systemctl disable --now skopos-gateway
```

ClickHouse schema changes are not automated by systemd. Apply the chart's
generated DDL or a reviewed migration before restarting a new gateway version.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `readyz` returns 503 | ClickHouse URL/credentials, `clickhouse-client` connectivity, firewall. |
| `permission denied` on env file | `chown root:skopos` and `chmod 0640`. |
| service restarts repeatedly | `journalctl -u skopos-gateway -n 100 --no-pager`; check config validation. |
| auth returns 401 | env is loaded (`systemctl show skopos-gateway -p Environment`), credentials match, client sends `Authorization`. |
| bind fails | `SKOPOS_LISTEN_ADDR`, port conflict, or `ProtectSystem`/network namespace settings. |
| OBI has no data | OBI/Collector is a separate deployment; check its privileges, BTF/tracefs, and Collector endpoint. |

## Security notes

- The gateway runs as an unprivileged system user with an empty capability set.
- Basic auth is one shared credential and has no per-user audit identity.
- TLS is required for Basic auth. Use a reverse proxy with a valid certificate.
- Keep `/etc/skopos/*.env` root-owned and group-readable only.
- ClickHouse credentials should be a dedicated read-only user, not `default`.
- For secret injection, systemd `LoadCredential=` is stronger than an env file.
  The gateway currently reads env variables, so a small wrapper is needed if you
  use credentials instead of an EnvironmentFile.
