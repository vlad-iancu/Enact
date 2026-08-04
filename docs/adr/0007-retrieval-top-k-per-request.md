# ADR-0007: RAG retrieval top-k is a per-request API parameter, not service config

**Date**: 2026-08-01
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

How many RAG chunks to retrieve per query started as a service-wide env
variable (`RAG_RETRIEVAL_TOP_K`). But the right value depends on the query
and the caller's cost/quality trade-off, not on the deployment.

## Decision

`POST /v1/inference` accepts an optional `retrieval_top_k` (1–50, default 5).
The default and the cap are code-level constants in the inference service;
the env variable is gone. Values outside the range are rejected with a 400 —
the cap exists so one request cannot pull an unbounded number of chunks into
the prompt.

## Alternatives Considered

### Alternative 1: Keep env config, add request override on top
- **Pros**: per-deployment tunable default
- **Cons**: two sources of truth for one knob; unclear precedence; more config surface
- **Why not**: nobody tunes this per deployment; per-request expressiveness is the actual need

## Consequences

### Positive
- Callers tune retrieval per query; API is self-describing (validated, documented in Swagger via the request type)

### Negative
- Changing the default or cap requires a code change and redeploy