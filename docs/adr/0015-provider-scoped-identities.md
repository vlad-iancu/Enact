# ADR-0015: Identities are scoped to a provider, not to a consumer

**Date**: 2026-08-14
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru

## Context

ADR-0014 keyed a stored credential by `(user, provider, application)` and had
the tool loop look one up at `application = mcp/<server-id>`. The intent was
blast-radius control: a credential connected for one MCP server was invisible
to another.

In practice it made the common case worse. A user whose agent uses two MCP
servers that both talk to GitHub was sent through the GitHub consent screen
twice and ended up with two copies of the same token, each refreshing
independently. Nothing in the platform could tell the two apart, and nothing
ever asked it to — no consumer other than the MCP tool loop ever set an
application at all.

## Decision

An identity is `(UserID, Provider)`. A user authenticates to a provider once,
and every consumer that declares a requirement for that provider — any MCP
server, present or future — uses that one credential. The `application`
concept is removed from the domain, the API, the events and the index.

Because one credential now serves several consumers, an OAuth
re-authorization requests the **union** of the access already granted and the
access newly asked for, so connecting from one server's prompt cannot narrow
what another relies on.

## Alternatives Considered

### Alternative 1: keep per-consumer scoping (ADR-0014 as written)
- **Pros**: a compromised or hostile MCP server only ever sees credentials a
  user connected specifically for it.
- **Cons**: the user connects the same account once per server; N copies of
  one token, refreshed N times; and the platform cannot answer "is this user
  connected to GitHub?" without knowing every server id.
- **Why not**: the isolation was theatre for the threat it named. Server
  registration is already a platform-level action, and a registrant who can
  declare a requirement can equally declare it against a per-server
  application. It cost real usability for a boundary that a hostile
  registrant walks around.

### Alternative 2: shared by default, per-server opt-in
- **Pros**: keeps an escape hatch for a server nobody wants to trust with the
  shared account.
- **Cons**: two lookup paths in the tool loop, two connect flows in the UI,
  and a per-server flag whose correct value no user can reason about.
- **Why not**: no caller asked for it. Reintroduce it if a concrete server
  ever needs it, rather than carrying the branch now.

### Alternative 3: keep `application` as descriptive metadata
- **Why not**: a field outside the key that nothing reads is a field that
  drifts. Either it identifies the credential or it does not exist.

### Alternative 4: last-write-wins on re-authorization
- **Pros**: the recorded access matches exactly what the newest token
  carries, with no inference.
- **Why not**: with a shared identity, one server's "connect at read" prompt
  would silently break another server's admin-level tools. The union is
  requested at the consent screen, so what is recorded is still only what the
  provider actually granted — `persistIdentity` remains authoritative.

## Consequences

### Positive
- One account, one connect, one refresh, regardless of how many servers use
  it.
- **Access levels stay a vocabulary, not a hierarchy.** A provider names
  levels (`read`, `readwrite`) and the concrete scopes each stands for;
  `ResolveAccess` expands a level into scopes and `HasAccess` checks the
  stored grant covers them. Nothing compares labels, and the platform invents
  no ordering — `readwrite` satisfies a `read` requirement only because its
  definition repeats read's scopes. This is what makes a credential obtained
  at one level usable by a requirement written at another, and what makes the
  union above safe: an identity can be labelled `read` while carrying more.
  A requirement may also name **no** level, meaning "the user has connected
  this provider" with no coverage check — the whole contract of a credential
  that has nothing to say about scope. Provider registration mirrors that
  asymmetry: a PAT provider needs no levels at all, because the user pastes a
  token whose scope the platform can neither see nor influence, while an
  OAuth provider must declare scopes or levels, because consent is a list of
  permissions shown to a person.
- `GET /identities` answers "what is this user connected to?" directly, and
  `GET /agents/{id}/required-identities` returns one entry per (provider,
  access level) with the `servers` that need it — one prompt per account.
- The waiter key, the Redis event and the doc id all shrink to the pair,
  removing a whole class of "same credential, different application string"
  bugs.

### Negative
- **Registering an MCP server is now enough to reach a user's credential**
  for any provider it declares in `tool_access_requirements`, with no
  per-server consent step. Server registration is the trust boundary; it is
  an admin/owner action, and the tool loop still only sends the providers a
  server declares.
- Disconnecting a provider revokes it for every server at once — correct, but
  a bigger action than the old per-server disconnect, and the UI should say
  so.
- An access level recorded on the identity can be narrower than the scopes it
  actually carries (connect at `read` over an existing `admin` keeps admin's
  scopes but labels the identity `read`). Harmless because coverage is always
  checked by concrete scopes via `ResolveAccess` + `HasAccess`, never by
  label equality — but the label is a hint, not a contract.

### Risks
- The document id is `sha256(user \x00 provider)`, so identities stored under
  the old triple are unreachable. There is no migration: the
  `enact-identities` index is deleted and recreated, and users reconnect.
- Superseded parts of [ADR-0014](0014-per-user-credentials-for-mcp-tools.md):
  its `application = mcp/<server-id>` lookup. Everything else in it — the
  caller's credentials rather than the owner's, injection in inference, the
  `X-Enact-Tool-Auth` envelope, pub/sub resumption — stands.

---

## Amendment (2026-08-16): providers are organization-scoped

Organizations (ADR-0019) made a provider an organization's configuration
rather than the platform's. The record is keyed `<organization>:<name>` and
carries `organization_id`, so the same provider name may exist in several
organizations and a lookup in one never finds another's.

Two consequences follow from that, both recorded here because they change who
may do what:

- **Registration moved off the administrator surface.** It is now
  `POST /identities/providers/oauth|pat` and
  `DELETE /identities/providers/{name}`, gated by `enact:provider:create` and
  `enact:provider:delete:<name>`. Keeping it administrator-only would have
  left every organization but the administrator's unable to obtain a provider
  at all. An organization owner passes by owner bypass and may delegate the
  rule through a role.
- **Registering records ownership**, exactly as creating an agent or a
  knowledge base does, and deleting revokes it. Without this a delegated
  registrar could create a provider they were not permitted to remove.

A stored identity carries `organization_id` too. It is not an authorization
input — access to a credential is still decided by the user it belongs to —
but the refresh sweep runs with no caller and would otherwise need a
membership lookup per credential to know which provider record to refresh
against.

