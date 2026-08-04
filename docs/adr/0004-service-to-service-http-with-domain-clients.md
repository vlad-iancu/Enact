# ADR-0004: Services are sources of truth, called over HTTP via domain-package clients

**Date**: 2026-08-02
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

Originally every service read whatever OpenSearch indices it needed: the
agent API validated KBs by reading the KB index, inference loaded agent
records and KB documents straight from storage. That blurred ownership — a
schema or business-rule change in one domain required touching every reader.

## Decision

Each domain's owning service is the source of truth, and other services call
its HTTP API. Client wrappers live in the domain packages (`kb.Client` in
`internal/kb`, `agents.Client` in `internal/agents`), never in the service
packages, and are built on the trace-propagating `requesthelper.Client()`.
Current call graph: agent-api → kb-api (validate KB refs), inference →
agent-api (resolve agent), inference → kb-api (fetch KB document contents).
Exception: inference's RAG chunk retrieval (k-NN vector search) stays a
direct OpenSearch read — it is inference's own retrieval engine, and
shipping embedding vectors over HTTP per query would be pure overhead.

## Alternatives Considered

### Alternative 1: Shared repositories (status quo)
- **Pros**: no network hop, no availability coupling
- **Cons**: no ownership boundary; every reader couples to index schemas; cross-cutting rules (e.g. filtering) duplicated
- **Why not**: source-of-truth ownership and independent evolution outweigh the hop cost at this scale

### Alternative 2: Clients inside the service packages (e.g. `enactkbapi.Client`)
- **Pros**: co-located with the API they call
- **Cons**: callers would import another service's internals; violates the repo's package-layout rule
- **Why not**: domain packages are the shared surface; service packages stay private

## Consequences

### Positive
- Clear ownership; response shapes are the contract, storage is private to the owner
- Cross-service calls are traced for free, giving real multi-service traces in Tempo

### Negative
- Availability coupling: agent inference now needs agent-api and kb-api up; latency adds ~10–30 ms per hop locally
- `KB_API_URL` / `AGENT_API_URL` configuration must be correct per environment

### Risks
- N+1 call patterns if used carelessly (e.g. per-KB document fetches) — acceptable at current fan-out, revisit with batching endpoints if it grows