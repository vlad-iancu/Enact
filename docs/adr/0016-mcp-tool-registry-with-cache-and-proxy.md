# ADR-0016: MCP servers are registered in a registry service that caches their tools and proxies their traffic

**Date**: 2026-08-15
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru

## Context

Agents call tools on MCP servers the platform does not own. Two problems fall
out of that, and they pull in different directions.

The inference service needs every tool's name, description and JSON schema
*before* it calls the model — Bedrock's `toolConfig` is part of the request,
not something that can be fetched mid-turn. Listing tools live would mean one
MCP handshake per referenced server on every single inference, and a server
that is slow or down would fail the whole request rather than one tool call.

Separately, tool calls have to leave the platform. Whoever makes that call is
the egress point for third-party traffic: it decides what headers cross the
boundary, and it is the only place that can stop the platform's own service
token from being handed to a stranger.

## Decision

`enact-tool-registry` owns two indices: `enact-tool-servers` (the records) and
`enact-tool-cache` (the tools those servers advertise). Registration
**probes** the server — connect, initialize, list tools — and refuses to store
a server it cannot reach. A background sweep re-probes every server on
`REFRESH_AT` (default 5m) and replaces the cache, keeping the previous tools
when a server is unreachable.

Every MCP exchange goes **through the registry's proxy**
(`/v1/servers/{id}/mcp`, `/sse`, and a catch-all for transport session
paths), a spec-compliant pass-through that rewrites SSE `endpoint` events to
stay inside the proxy, strips the platform's own credentials, and expands the
`X-Enact-Tool-Auth` envelope onto the upstream request (ADR-0014).

## Alternatives Considered

### Alternative 1: list tools live on every inference
- **Pros**: never stale; no cache to invalidate; a removed tool disappears
  immediately.
- **Cons**: an MCP handshake per server per request, on the latency path of
  every message; one slow third party degrades every inference; a server that
  is down takes the whole request with it rather than one tool.
- **Why not**: the tool list changes on the order of deployments, not
  requests. Staleness is bounded by `REFRESH_AT` and costs at worst one failed
  tool call, which the model is told about and can work around.

### Alternative 2: let the inference service dial MCP servers directly
- **Pros**: one less hop, one less service in the path of a tool call.
- **Cons**: every service that calls a tool would have to reimplement
  transport handling, and — the real problem — each would be its own egress
  point. `internal/s2s` sets `Authorization` as the innermost RoundTripper, so
  a direct call would hand the platform's service token to a third party
  unless every caller remembered to strip it.
- **Why not**: one boundary, enforced in one place, beats a rule everyone
  must remember. The proxy strips `Authorization` and `X-User-Id`
  unconditionally.

### Alternative 3: store tool definitions on the server record
- **Why not**: they are refreshed on a different cadence than the record is
  written, and a tool catalogue can be large. A separate index keyed
  `<server>::<tool>` lets the sweep replace one server's tools without
  rewriting its registration.

## Consequences

### Positive
- Building a request's tool config is a cache read, not a network fan-out.
- One egress point for third-party MCP traffic, so credential stripping and
  envelope expansion are implemented once.
- Because the proxy is spec-compliant, an ordinary MCP client can point at
  the platform and reach a registered server — which is why the proxy routes
  also accept anonymous callers, since such clients cannot sign platform
  tokens.
- Registration fails loudly for an unreachable server rather than storing a
  record whose tools never appear.

### Negative
- The catalogue is stale for up to `REFRESH_AT`. A tool added upstream is
  invisible until the sweep runs; a tool removed upstream is advertised to
  the model and fails when called.
- The sweep re-probes every server on a fixed interval regardless of use, so
  an unused registration still costs a handshake every five minutes.
- A server must be reachable at registration time, which makes registering a
  server that is temporarily down impossible even when the configuration is
  correct.

### Risks
- **The proxy routes accept anonymous callers.** Anyone who can reach the
  registry can proxy to any registered server. For a server that requires no
  credentials, that is unauthenticated access to its tools through the
  platform. It is deliberate — spec-compliant MCP clients cannot present a
  service token — but it means the registry must not be exposed publicly
  without an authenticating layer in front.
- The refresh sweep keeps cached tools when a probe fails, so a server that
  has been decommissioned still advertises tools until its record is deleted.
  Chosen over emptying the cache, which would silently disarm every agent
  using it during a transient outage.
