# ADR-0009: Each service owns exactly one domain and does not trespass

**Date**: 2026-08-02
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

The platform is split into five services. Early on, boundaries were porous:
services read each other's OpenSearch indices and duplicated domain logic
(agent-api validating KBs against the KB index, inference loading agent
records and KB documents from storage). ADR-0004 established *how* services
talk (HTTP via domain clients); this ADR fixes *what each service is for*.

## Decision

Every service has a single domain of activity and stays inside it. A service
never implements another domain's rules, never reads another domain's
storage, and obtains anything outside its domain by calling the owning
service's API.

| Service | Domain — and nothing else |
|---------|---------------------------|
| `enact-model-management` | Catalogue of available models: friendly name → Bedrock model id mapping. |
| `enact-kb-api` | Knowledge bases: KB lifecycle, context-document upload (enqueue for indexing), serving KB metadata and document contents. |
| `enact-kb-document-indexer` | Asynchronous document processing: consume the queue, extract text (Tika), store KB documents whole, chunk + embed agent RAG documents. No public API. |
| `enact-agent-management-api` | Agents: agent lifecycle (model, system prompt, KB references), RAG document upload (enqueue). Source of truth for agent records. |
| `enact-model-inference` | Inference orchestration: resolve the agent via agent-api, assemble context via kb-api, perform RAG retrieval over its chunk index, call Bedrock, stream/return results. Owns no CRUD. |

Shared *code* (repositories, clients, types) lives in domain packages
(`internal/kb`, `internal/agents`, …), but shared code is not shared
*authority*: the owning service remains the only writer and the only public
reader of its domain.

## Alternatives Considered

### Alternative 1: Modular monolith (one service, internal packages as boundaries)
- **Pros**: no network hops, single deployment, simpler local dev
- **Cons**: boundaries erode without a process boundary enforcing them (empirically: the index-sharing that motivated this ADR); no independent scaling of the indexer
- **Why not**: the platform is explicitly an exercise in service architecture with observability across boundaries

### Alternative 2: Boundaries by technical layer (api / storage / workers)
- **Pros**: familiar layering
- **Cons**: every feature cuts across all layers; no single owner per business concept
- **Why not**: domain ownership keeps a change (e.g. KB document shape) inside one service

## Consequences

### Positive
- A domain change has one home; cross-domain access is visible in traces as HTTP calls
- Responsibilities are auditable: an import of another domain's repository in a service package is a boundary violation by definition

### Negative
- More moving parts locally (five processes + infra + LGTM); availability coupling between services (see ADR-0004)

### Risks
- Boundary drift as features grow — mitigated by this table being the reference: if a change doesn't fit a service's one-line domain, it belongs elsewhere (or the table needs a deliberate, recorded amendment)