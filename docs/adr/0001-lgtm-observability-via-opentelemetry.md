# ADR-0001: LGTM observability stack wired via OpenTelemetry

**Date**: 2026-08-01
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

The platform runs five cooperating services; debugging a request (e.g. "why
didn't the agent read its knowledge base?") requires correlating logs, the
request path across services, and latency data. Plain stdout logs per service
cannot answer cross-service questions.

## Decision

We use the Grafana LGTM stack (Loki logs, Grafana UI, Tempo traces, Mimir
metrics), fed directly from each service via OpenTelemetry over OTLP/HTTP —
one exporter per signal, no collector in between (`internal/telemetry`).
Providers are installed as OTel process globals; instrumentation code depends
only on the OTel API, and `OTEL_SDK_DISABLED=true` degrades everything to
no-ops so services run fine without the stack.

## Alternatives Considered

### Alternative 1: OTel Collector between services and backends
- **Pros**: central place for sampling/routing; services need one endpoint
- **Cons**: one more container and config surface for local dev
- **Why not**: at five services and one host, per-signal direct export is simpler; a collector can be inserted later without touching service code

### Alternative 2: Prometheus + ELK (separate metric and log stacks)
- **Pros**: widely known tooling
- **Cons**: no first-class trace correlation; two UIs; heavier footprint
- **Why not**: log ↔ trace ↔ metric cross-linking in one Grafana UI was the primary goal

## Consequences

### Positive
- One UI (Grafana, anonymous admin locally) for all three signals with click-through correlation
- Telemetry is best-effort: exporters buffer and the stack being down never blocks serving traffic

### Negative
- Local dev runs four extra containers (`make observability-up`)
- Export is asynchronous (batching); telemetry lags reality by ~1–5 s and can drop on overflow

### Risks
- Grafana/plugin/backends version coupling (e.g. Logs Drilldown app requires Grafana ≥ 11.6) — pin images in `deploy/docker-compose.lgtm.yml` and upgrade deliberately