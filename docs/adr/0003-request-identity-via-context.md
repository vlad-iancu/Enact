# ADR-0003: Request identity flows through context, not parameters

**Date**: 2026-08-02
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

The platform's stub auth identifies the caller by the `X-User-Id` header
(default user when absent; real authn/authz intentionally out of scope).
Handlers, repositories, and service clients all need the user id; threading
it as an explicit parameter spread it through every signature.

## Decision

The user id travels on `context.Context`, mirroring how trace context flows.
`identity.Filter` (registered container-wide) resolves the header once per
request; `identity.FromContext` reads it anywhere downstream; outbound
service clients forward it as the header automatically. Acting on behalf of
another user (e.g. inference loading KBs as the agent's owner) is explicit
via `identity.WithUserID`.

## Alternatives Considered

### Alternative 1: Explicit `userID string` parameters everywhere
- **Pros**: visible in signatures; no context "magic"
- **Cons**: noise in every call chain; easy to pass the wrong value; already caused duplicated `identity.UserID(req)` calls
- **Why not**: identity is ambient request state, exactly what Go context is for — same rationale as trace propagation

## Consequences

### Positive
- Signatures shrink; impersonation across service calls is automatic and consistent
- One place (the filter) defines how identity is resolved, easing a future switch to real auth

### Negative
- The dependency is invisible in signatures; code called outside a request context silently gets the default user

### Risks
- Forgetting the filter in a new runtime path would default everyone to `default` — mitigated by registering it once in `internal/service`