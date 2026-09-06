# Local observability runbook (Week 10 A3)

Self-hosted local observability stack: Gateway (host) → OTLP gRPC → Collector
(contrib) → Tempo; Prometheus pulls the Gateway's private Ops `/metrics`;
Grafana queries Prometheus + Tempo. All host-published ports bind `127.0.0.1`.

## Topology

```
Gateway (host, OPS_ADDR=:9090, /metrics)
   │ OTLP gRPC (push) 127.0.0.1:4317
   ▼
OpenTelemetry Collector (contrib) ──otlp──► Tempo (traces, :3200)
Prometheus ── pull host.docker.internal:9090/metrics ──► Gateway ops
   │                                                       (metrics)
   └────► Grafana (:3001)  datasources: Prometheus + Tempo
```

## Commands

- `make observability-up` — start collector + tempo + prometheus + grafana and
  poll readiness host-side (Tempo `/ready`, Prometheus `/-/ready`, Grafana
  `/api/health`).
- `make observability-ps` / `make observability-down`.
- `make observability-evidence` — traces round trip: OTLP gRPC export →
  Collector → Tempo → Tempo query API (integration test, requires the stack).
- `make observability-metrics-evidence` — full metrics loop: starts postgres +
  stack + a real gateway binary (data `:18080`, control `:18081`, ops `:9090`,
  same `.env`), seeds a project/key/credential pointing at an in-test mock
  OpenAI endpoint, fires a real request, and asserts `gateway_requests_total`
  is visible through the Prometheus API **and** the Grafana Prometheus
  datasource, with the auto-provisioned Gateway Overview dashboard importable.

## Evidence steps (manual)

1. `make observability-metrics-evidence` (traces are covered separately by
   `make observability-evidence`; run both).
2. Grafana: open `http://127.0.0.1:3001` (local admin), Datasources show
   Prometheus + Tempo; Dashboard → Gateway Overview (minimal) shows the
   gateway request rate and active requests after traffic.
3. Prometheus API checks:
   - `curl http://127.0.0.1:9091/api/v1/query?query=up`
   - `curl 'http://127.0.0.1:9091/api/v1/query?query=sum(gateway_requests_total)'`

## Notes / deferred

- Metrics are pulled by Prometheus only; they never enter the Collector.
- pprof (`PPROF_ENABLED`/`PPROF_TOKEN`) and richer dashboards land in later
  slices.
- The images are distroless; container readiness is verified host-side rather
  than via in-container `HEALTHCHECK` (the compose readiness polling above).
- Grafana admin credentials are local-dev only via env interpolation
  (`GRAFANA_ADMIN_USER`/`GRAFANA_ADMIN_PASSWORD`, default `admin`).
