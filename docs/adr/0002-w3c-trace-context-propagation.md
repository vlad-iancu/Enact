# ADR-0002: W3C trace context propagation via middleware and wrapped HTTP client

**Date**: 2026-08-01
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

For a request that crosses service boundaries to appear as one trace, the
trace identity must travel with the request. The initial idea was a homegrown
`traceId` context key copied into a custom header by a shared HTTP client.

## Decision

We propagate the standard W3C `traceparent`/`tracestate` (+ `baggage`)
headers using OpenTelemetry propagators. `internal/requesthelper` owns both
halves: `TracingFilter` (server side — extracts headers, starts a server
span, enriches the request context) registered container-wide by the service
runtime, and `NewTransport`/`Client` (client side — starts a client span,
injects headers). All service-to-service calls must use the wrapped client.

The same propagation crosses the async queue boundary: `queue.Producer`
injects the caller's trace context into each `DocumentMessage` (a JSON map
carrier), and the consumer extracts it and dispatches the handler under a
consumer span — so document indexing joins the trace of the upload request
that queued it.

## Alternatives Considered

### Alternative 1: Custom `X-Trace-Id` header + context key
- **Pros**: trivially simple, no OTel dependency in the transport
- **Cons**: incompatible with Tempo/Grafana correlation, samplers, and any third-party tooling; reinvents a solved standard
- **Why not**: the standard form is the same idea and makes the whole LGTM correlation work out of the box

## Consequences

### Positive
- Cross-service traces (inference → agent-api, inference → kb-api) reconstruct automatically in Tempo
- Sampling decisions inherit correctly (`ParentBased`), avoiding shredded traces

### Negative
- Any HTTP call made with a bare `http.Client` silently breaks the trace — discipline required (use `requesthelper.Client()`)

### Risks
- Middleware ordering matters (tracing filter must run first); owned centrally in `internal/service` so individual services cannot get it wrong