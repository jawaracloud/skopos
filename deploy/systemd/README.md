# systemd deployment files

These files run the Skopos gateway on a plain Linux host with systemd. The
gateway is read-only and stateless; ClickHouse, the OpenTelemetry Collector, and
OBI remain separate components.

Start with [docs/systemd.md](../../docs/systemd.md) for the full install,
security, upgrade, and troubleshooting guide.

| File | Purpose |
| --- | --- |
| `skopos-gateway.service` | Single process: three query heads and public API. |
| `skopos-query.service` | Internal query worker for `SKOPOS_MODE=query`. |
| `skopos-frontend.service` | Public proxy for `SKOPOS_MODE=frontend`. |
| `gateway.env.example` | Single-process environment file. |
| `query.env.example` | Query-worker environment file. |
| `frontend.env.example` | Frontend environment file. |
| `render-schema.sh` | Render the chart's local-only ClickHouse DDL. |

The default listener is loopback only. Terminate TLS at a reverse proxy and use
the optional Basic auth when exposing the API.
