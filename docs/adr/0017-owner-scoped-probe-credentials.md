# ADR-0017: A gated MCP server is reached with its owner's credentials

**Date**: 2026-08-15
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru

## Context

ADR-0014 assumed a server's tool list could be read anonymously, and listed
the opposite as a known risk: *"credential-gated servers cannot be
registered"*. GitHub's MCP server is exactly that case — its HTTP mode
answers 401 from a chi middleware mounted above the router, so `initialize`
and `tools/list` are refused before the JSON-RPC body is parsed. No amount of
per-tool credential configuration helps, because the platform never gets far
enough to name a tool.

So the platform needs credentials of its own for the handshake. Whose?
Registration has a user in scope (the registrant). The background refresh
sweep (ADR-0016) has nobody at all: it runs on a timer against every stored
server.

## Decision

`tool_access_requirements` and `tool_authorizations` accept two further keys,
**`/initialize`** and **`/list-tools`**, configured exactly like a tool's.
They lead with `/` because an MCP tool name never does, so they cannot
collide with one. Declaring one phase covers both — a server that gates its
handshake gates everything after it.

They resolve against the server's **owner**, everywhere: at registration, on
the refresh sweep, and inside a caller's agent session, where the owner's
headers are merged *underneath* the caller's so a tool's own `Authorization`
wins a name clash. They are the key to the door; the caller's own credentials
decide who they are once inside.

They never inherit the `"*"` wildcard.

## Alternatives Considered

### Alternative 1: resolve probe keys against the calling user
- **Pros**: no credential ever crosses users; identical to how tool
  credentials work, so one rule covers everything.
- **Cons**: the refresh sweep has no calling user, so it would need a second
  mechanism anyway — and a gated server would be unusable by anyone who has
  no account at that third party, even for tools that need no credential.
- **Why not**: it makes the common case (an admin registers a gated server
  for a team) impossible, and still leaves the sweep unsolved.

### Alternative 2: probe keys apply only to the registry
- **Pros**: the smallest change, and no owner credential ever appears in a
  caller's session.
- **Cons**: an agent session opens its own MCP session, so a gated server
  whose tools declare no credentials stays unusable from an agent — the
  server registers, its tools appear, and every call fails at the handshake.
- **Why not**: registering a server the platform cannot then use is a
  half-measure.

### Alternative 3: let the wildcard cover probes
- **Why not**: `"*"` is about tools. Inheriting it would mean registering any
  wildcard server quietly starts spending the owner's credential on platform
  traffic — a credential use nobody wrote down.

## Consequences

### Positive
- Credential-gated servers — GitHub's, Google's Workspace family — can be
  registered, refreshed and called.
- The refresh sweep has a well-defined identity (`server.Owner`) instead of
  running as nobody.
- Registration and any update that changes the probe configuration re-probe
  the server, so fixing a broken credential is proved rather than assumed.
- Per-tool credentials are unaffected: they remain the caller's.

### Negative
- **An agent can reach a gated server on its owner's access.** That is what
  registering a gated server on someone's behalf means, and it is the price
  of the server being usable at all — but it is a real widening, and the
  owner is choosing it for every user of every agent that references the
  server.
- A server's registration now depends on a credential that can expire or be
  disconnected. When it does, the sweep starts failing and the tool catalogue
  quietly stops updating (the cached tools are kept, per ADR-0016).
- Two more keys to explain, and a reader who sees `/initialize` in a map of
  tool names has to be told it is not a tool.

### Risks
- The owner's credential is resolved fresh on every probe and every session
  open, so a revoked credential surfaces as a failed handshake rather than a
  clear message. The registry logs the reason; a caller sees a tool call
  fail.
- Nothing prevents an owner from configuring a probe credential with far more
  access than the handshake needs. The platform cannot tell the difference
  between a gate key and a powerful account.
