# ADR-0005: Every JSON response carries a `meta` block (trace_id + execution time)

**Date**: 2026-08-02
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

Callers reporting a problem had no handle to hand back to operators; finding
"their" request in the observability stack required guessing by timestamp.

## Decision

All JSON API responses — success and error — include a top-level
`meta` object: `trace_id` (the request's distributed trace) and
`execution_time_ms` (measured from request arrival, stamped by the tracing
filter). Handlers write responses exclusively through
`requesthelper.WriteJSON` / `WriteError`, which inject the block centrally.
SSE streaming responses carry the same information differently: an
`X-Trace-Id` response header plus an initial `event: meta` SSE event (the
browser EventSource API cannot read headers), and the `event: error` payload
embeds the meta block. 204s are exempt (no body to decorate).

## Alternatives Considered

### Alternative 1: `Meta` field added manually to every response struct
- **Pros**: visible in Swagger models; no map-merge step
- **Cons**: ~15 structs plus every write site to keep consistent by hand; easy to forget on new endpoints
- **Why not**: central injection makes the invariant structural instead of disciplinary

### Alternative 2: Response headers (`X-Trace-Id`, `Server-Timing`)
- **Pros**: no body mutation; standard-ish
- **Cons**: invisible in JSON tooling and copy-pasted payloads, which is where users actually look
- **Why not**: the point is that a pasted response body alone is enough to find the trace

## Consequences

### Positive
- Any response can be traced: paste `meta.trace_id` into Tempo / Loki
- New endpoints inherit the behavior by using the shared writers

### Negative
- Swagger models don't show `meta` (route `Returns` still reference the plain structs)
- Payloads must marshal to JSON objects for injection (all current ones do)