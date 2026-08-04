# ADR-0008: All code paths log all non-sensitive parameters and relevant intermediary results

**Date**: 2026-08-02
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

Debugging the "agent ignores its knowledge base" incident stalled because the
decisive fact — the document lookup returned zero files — was logged at
Debug level while services run at Info. The observability stack was in place,
but the signal that mattered never reached it.

## Decision

Every code path logs, as structured fields, **all of its non-sensitive
parameters and all intermediary results that might be relevant for
debugging** — and these records must be emitted at a level that is visible
under the normal operating configuration (Info), not hidden behind Debug.

Concretely, every method (HTTP handlers included) logs **at entry** with the
non-sensitive request parameters available at that point (path/query ids;
body parameters logged immediately after decode), and **after every block
that can fail** (lookup, decode, validation, external call, store). The last
emitted line then brackets exactly where a crash or hang occurred.
Examples of intermediary results that must be logged: counts and sizes of
loaded documents/chunks, resolved model ids, retrieval parameters, upstream
token usage, branch decisions (e.g. "KB missing, degrading to no context").
Sensitive values — credentials, API keys, raw document text, full prompts,
user message content — are never logged; log their sizes or hashes instead.

## Alternatives Considered

### Alternative 1: Keep detail at Debug, enable Debug when investigating
- **Pros**: quieter logs, marginally cheaper
- **Cons**: incidents are investigated after the fact; the data needed was not captured at the time — exactly the observed failure
- **Why not**: retroactively enabling Debug cannot recover a past request's context

### Alternative 2: Sample verbose logs (e.g. only errors carry detail)
- **Pros**: bounded volume
- **Cons**: "why did this *succeed* wrongly" cases (empty result sets, silent skips) carry no error
- **Why not**: the incident that motivated this ADR was a non-error path

## Consequences

### Positive
- Any past request can be reconstructed from Loki via its trace_id without reproducing it
- Log volume stays structured (fields, not prose), so Loki queries remain cheap

### Negative
- Higher log volume; local dev retention (24h, dev-scale Loki) absorbs it — revisit sampling/retention if deployed at scale
- Reviewers must police the sensitive/non-sensitive line on every new log line

### Risks
- Accidental leakage of sensitive values into logs — mitigated by the explicit never-log list above and code review; log sizes/counts, not contents