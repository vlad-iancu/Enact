# ADR-0014: Per-user credentials for MCP tool calls

**Date**: 2026-08-13
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru

## Context

Agents can call MCP tools (ADR-0009's tool registry) and the platform can
store users' third-party credentials (ADR-0013), but nothing joined the two:
the tool loop called every MCP server anonymously. Real servers do not work
that way — the GitHub MCP server wants `Authorization: Bearer ghp_…` on
every request, and other servers expect an API key as a call argument.

Four questions had to be answered: whose credential, who injects it, how it
survives the hop to the server, and what happens when the user has not
connected the account yet.

## Decision

A registered MCP **server** declares, per tool, what credentials the tool
needs (`tool_access_requirements`) and how to present them
(`tool_authorizations`, Go `text/template` over the resolved credentials).
The **inference** service resolves the **calling user's** credentials from
`enact-external-identities` (originally under `application = mcp/<server-id>`;
[ADR-0015](0015-provider-scoped-identities.md) replaced that with one identity
per provider), renders
them, injects params into the tool arguments and headers into an envelope
the **registry proxy** translates onto the upstream request. When a
credential is missing, the tool call parks, emits
`toolCallWaitingAuthorization`, and resumes when a **Redis pub/sub** event
from the identity service says the user connected it.

> **Amended 2026-08-17 — the waiting is gone.** A missing credential now
> fails the call immediately; see
> [Amendment: tool calls no longer wait](#amendment-2026-08-17-tool-calls-no-longer-wait)
> at the end of this ADR. Everything else here still holds: the caller's
> credentials, resolved at call time, injected through the proxy envelope.

## Alternatives Considered

### Alternative 1: use the agent owner's credentials
- **Pros**: one shared service account per agent; matches how knowledge
  bases are loaded (`identity.WithUserID(ctx, agent.UserID)`).
- **Cons**: every user of an agent acts as its author at the third party,
  and the waiting flow becomes incoherent — the person at the keyboard
  cannot fix a credential they do not own.
- **Why not**: a tool acts on behalf of the person who asked. KBs are the
  agent's *configuration*; a GitHub token is the *caller's* identity.

### Alternative 2: inject credentials in the registry proxy
- **Pros**: the proxy already holds the server record and is the single
  egress point.
- **Cons**: the proxy forwards opaque JSON-RPC bytes and does not know
  which tool an exchange concerns, so per-tool injection would require
  parsing (and buffering) the protocol stream.
- **Why not**: the decision belongs where the tool is known. The proxy
  still owns the *boundary* concerns: translating the envelope and
  stripping platform headers.

### Alternative 3: send credentials under their real header names
- **Why not**: impossible on this hop. `internal/s2s`'s transport is the
  innermost RoundTripper and overwrites `Authorization` with the platform's
  own service token. Hence one opaque envelope header,
  `X-Enact-Tool-Auth`, carrying base64url JSON that the proxy expands.

### Alternative 4: poll the identity service instead of pub/sub
- **Pros**: no new mechanism; replica-safe by construction.
- **Cons**: resumption latency equals the poll interval, and a user who
  just finished a consent screen is watching a spinner.
- **Why not**: chosen as the *safety net*, not the primary path. Redis
  pub/sub resumes in milliseconds (measured: 19ms end to end) and the
  ticker covers a dropped message.

## Consequences

### Positive
- Tools reach third parties as the user, with credentials that are never
  visible to the model, the client, or the conversation record.
- Validation renders each template against sentinel credentials through the
  same code path used at call time, so "it validated" means "it will
  render".
- Fixes an existing leak: the proxy now strips the platform's own S2S token
  and `X-User-Id` before forwarding to a third party.
- Rendered credentials win over model-supplied arguments of the same name,
  so a prompt-injected model cannot choose its own API key.

### Negative
- Provider names may contain `-`, which a Go template field selector cannot
  express: `{{ .my-provider.Credentials }}` is a *parse* error. The
  supported idiom is `{{ (cred "my-provider").Credentials }}`, and the
  validation message says so — but it is a papercut for anyone writing a
  template by hand.
- A tool call can now block for minutes (`TOOL_AUTH_WAIT_TIMEOUT`), holding
  an SSE connection open. The heartbeat re-announcement keeps intermediaries
  from closing it.
- Waiters are in-memory, so a service restart mid-wait abandons the call
  (the user retries). Acceptable: the waiter and its SSE connection live in
  the same process by construction.

### Risks
- ~~**Credential-gated servers cannot be registered.**~~ Resolved by
  [ADR-0017](0017-owner-scoped-probe-credentials.md): the `"/initialize"` and
  `"/list-tools"` keys carry the server OWNER's credentials through the
  platform's own handshake and tool listing. Per-tool credentials remain the
  caller's.
- **A hostile MCP server can echo an injected credential back** in its tool
  output, which reaches the model and the conversation record. Redacting
  known credential values from results before they leave the tool loop is
  the cheap mitigation, not yet implemented.
- The non-streaming inference path cannot wait (there is no channel to tell
  anyone what to connect), so it returns a tool error naming the provider
  instead of stalling silently.

---

## Amendment (2026-08-17): tool calls no longer wait

The parking mechanism is **removed**. A tool call whose credentials are not
connected fails at once, emitting `toolCallAuthorizationRequired` with the
same `missing` payload, followed by the call's ordinary `toolCallResult`
carrying `is_error: true`. The user connects the account and asks again;
nothing retries on their behalf.

Removed with it: the in-memory waiters, the recheck and heartbeat tickers,
`TOOL_AUTH_WAIT_TIMEOUT` / `TOOL_AUTH_RECHECK_INTERVAL` /
`TOOL_AUTH_HEARTBEAT_INTERVAL`, the whole `internal/identityevents` package,
its Redis publisher in enact-external-identities and its subscriber in
enact-model-inference.

**Why.** The original decision optimised the wrong moment. Parking holds a
model turn, an SSE connection and a Bedrock context open for up to five
minutes on the chance that somebody completes an OAuth flow in another tab —
paying a real cost on every refusal to save a retry on the few that get
connected. The consequences section below already listed most of it: calls
blocking for minutes, waiters lost on restart, the non-streaming path unable
to wait at all, so the same missing credential behaved differently depending
on how you asked.

The event contract was the tell. It needed a status machine
(`waiting`/`resolved`/`timeout`), a countdown, 30-second heartbeats to keep
proxies from closing the stream, and a re-announcement whenever a client
might have connected late — all of it protocol for a state that exists only
because the call refused to end.

**What replaces it.** `GET /agents/{id}/required-identities` answers the same
question *before* a conversation starts, which is where the prompt to connect
an account belongs. The runtime refusal is the backstop, not the workflow.

**Consequence.** A user whose credential is missing loses the turn: the
assistant reports that the tool could not run, and they ask again after
connecting. That is one extra round trip, in exchange for an inference
service that holds nothing open and a platform with no Redis pub/sub
dependency at all.

