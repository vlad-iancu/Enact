# ADR-0019: Organizations are the isolation boundary, and every resource stores which one it is in

**Date**: 2026-08-16
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru

## Context

The platform had no authorization model. A request was either "logged in" or
not, plus a single `ADMIN_EMAIL` super-user, plus a `user_id`/`owner` scalar
applied inconsistently per resource. The consequence was concrete: **any
logged-in user who knew an id could read, modify or delete another user's
agent or knowledge base**, because `agents.Get(ctx, id)` and `kb.Get(ctx, id)`
took no user and neither the gateway nor the owning service compared owners.
Only MCP servers and conversations checked anything at all.

Multi-tenancy also had nowhere to live. Users self-register; nothing grouped
them, so there was no unit to isolate, no one to administer a group, and no
way to say "these people share these agents" short of sharing an account.

This ADR records the model that closed both: **organizations** as the
isolation boundary, **roles** as the grant mechanism, and a permission
grammar every service evaluates for itself.

Two decisions inside it were made, reversed, and remade as evidence arrived —
they are recorded here with the evidence, because the reversal is the
interesting part.

## Decision

**An organization is the isolation boundary.** Every user belongs to exactly
one; every resource belongs to exactly one; nothing crosses. A user without an
organization can log in, see their profile, and submit an organization
request — nothing else. Organizations are created by an administrator
approving a request, and the requester becomes the first owner.

**Permissions are four colon-separated segments**, matched by segment-glob and
nothing else — no regex:

```
enact:<resource-type>:<action>:<resource-id>
enact:kb:edit:9f2a…        one knowledge base
enact:agent:view:*         every agent
enact:kb:*                 every action, every knowledge base
```

`*` matches one segment; a rule may stop early and cover what follows. A rule
with MORE segments than the permission never matches, so a narrow grant is
never inherited by a broader question.

**Roles hold rules and live inside an organization**, so the same role name may
exist in many. **A user's own id is a hidden role name**: creating a resource
grants `enact:<type>:*:<id>` under the role `<user-id>`. Ownership is therefore
expressed in the same grammar as everything else, with no second mechanism.
Hidden roles are excluded from listings, cannot be edited as roles, and their
names are reserved so no visible role can claim one.

**Conversations are the exception, and carry no rules at all.** They are
private to one person: the check is a comparison of the stored `user_id`
alongside `organization_id`, in `conversations.Repository.Get`, and no code
path evaluates an `enact:conversation:…` permission. Nothing can grant access
to somebody else's conversation, including an organization owner — the
permission bypass does not reach them, because no permission is consulted.

That is deliberate rather than incidental. A conversation holds the user's
own words and, since ADR-0018, the verbatim results of every tool call made
on their behalf — output pulled from *their* connected accounts. Making it
shareable by rule would mean an owner could read it, which is a different
product decision than "an owner administers the organization's resources".

The migration therefore does not grant conversation ownership rules, and the
ones written before this was settled were removed. A rule that governs
nothing is worse than no rule: it appears in a user's effective rules and in
`GET /organizations/me`, implying a grant the platform does not honour,
exactly when someone is asking "who can see what".

**Every service enforces, not just the gateway.** enact-main, the agent, KB,
tool-registry, identity and inference services each call the RBAC service and
evaluate locally. Downstream services are reachable by any signed service
caller (ADR-0011), so a single gateway check would be one hop from being no
check at all.

**An organization owner may do anything inside their own organization**, with
no rule saying so — but only inside it. Owner bypass exists because an
organization's first owner holds no rules at the moment of creation and could
otherwise create nothing; and because an owner edits the roles, so any
permission they lack they could grant themselves in one call. Enforcing rules
against them would add a step, not a safeguard.

**Every resource stores `organization_id`, and reads compare it.** Agents,
knowledge bases, conversations and MCP servers each carry the field; identity
providers and stored identities carry it too. The comparison lives in the
repository — `agents.Get(ctx, organizationID, id)` — and a record from another
organization is reported **absent**, not refused.

**Two resources are additionally keyed by organization**, because their names
are chosen by people rather than generated: identity providers
(`<org>:<name>`) and MCP servers (`<org>:<id>`). Their id namespace is
per-organization, so two organizations may each register `github`.

## Alternatives Considered

### Alternative 1: infer a resource's organization from its owner

Adopted first, then reversed. A user belongs to exactly one organization, so
`resource → user_id → membership → organization` is total, and it needed no
schema change, no migration, and no stamping.

- **Pros**: nothing to store, nothing to migrate, no field that can drift out
  of step with the membership it mirrors.
- **Cons**: correct only while every read passed a permission check that named
  the resource id AND rules came only from roles inside the caller's own
  organization.
- **Why not**: **owner bypass breaks the second premise.** `Allows()` returns
  true for any permission an owner asks about, so `RequireResource(view, id)`
  passed for an owner of *any* organization, and the read that followed fetched
  by id alone. An owner of organization B could read, edit and delete every
  agent and knowledge base in organization A. Inference was not wrong about
  where a resource belonged; it was never consulted.

### Alternative 2: keep inference, and check the owner's membership on each read

The narrower repair: leave the schema alone and, after loading a record, check
its owner is in the caller's organization using the enforcer's cached member
list.

- **Pros**: no migration; the member list is already cached for listings.
- **Cons**: the check is a step each of ~24 call sites must remember, and the
  one that forgets is invisible — exactly the failure that produced this ADR.
  Listings still needed `terms user_id = <every member>`, which grows with the
  organization.
- **Why not**: a boundary enforced by remembering is not a boundary. Storing
  the field let the comparison move into the repository, where no handler can
  omit it.

### Alternative 3: bound owner bypass by making owners hold explicit rules

Give an owner a role granting `enact:*` in their organization, and drop the
bypass entirely.

- **Pros**: one mechanism; the organization is part of every rule.
- **Cons**: the rules would have to be rewritten whenever a resource type is
  added, and a new organization's first owner would hold nothing until the
  grant landed.
- **Why not**: it moves the same "may do anything here" statement into data
  that must be maintained, without changing what it says.

### Alternative 4: one global namespace for MCP server ids

What existed: server documents keyed by the caller's chosen id.

- **Pros**: an id means the same thing everywhere; agent `tools: [...]`
  references need no qualification.
- **Cons**: the first organization to register `google-mcp-server` took the
  name from every other, and the refusal disclosed that a server the caller
  could not see existed.
- **Why not**: a name collision across a boundary that is supposed to be total
  is a leak, however small.

## Consequences

### Positive

- The hole that motivated this is closed: reads are scoped in the repository,
  so a resource of another organization is absent rather than merely
  unauthorized, and "not yours" is indistinguishable from "does not exist".
- Ownership, delegation and administration share one grammar. "Who can do
  what" is answerable by reading rules, and an owner can delegate anything
  they hold by putting it in a role.
- Listings became a single `term organization_id` instead of a `terms` query
  over every member id, and no longer grow with organization size.
- Two organizations can hold providers and MCP servers of the same name with
  different endpoints, clients and scopes.
- Defence in depth: a service reached directly, bypassing the gateway, still
  refuses.

### Negative

- `organization_id` is denormalized, and a membership that moved would leave
  every resource behind. Membership is therefore **immutable in its
  organization** — changing it is refused with a 409; remove and re-add
  instead.
- Authorization costs a round trip per user per cache window. The TTL is a
  staleness trade: a revoked role stays live for up to it.
- A cached denial is re-checked once against the service before being returned,
  because each service caches independently and a resource created elsewhere
  moments ago would otherwise be refused to its own creator. Refusals pay one
  extra call; grants pay nothing.
- Two documents changed identity — providers and MCP servers — which requires a
  migration before the services that resolve them run again.

### Risks

- **A missing `organization_id` must fail closed.** During migration a record
  may not yet carry one; the repositories treat empty as "belongs to nobody"
  and report it absent, so a half-migrated cluster refuses rather than leaks.
- **Owner bypass is still unconditional inside an organization.** If a
  resource type is added whose reads are not organization-scoped, an owner
  reaches it platform-wide again. The e2e case
  `TestRBAC_CrossOrganizationIsolation` exists to catch that: it stands up a
  second organization, makes a user its owner, and insists every resource of
  the first is 404 to them.
- **The ownership grant is a read-modify-write on one document per user.** It
  is applied as a scripted update so two resources created at the same moment
  cannot lose one another's rule — a plain get-then-index pair did, leaving a
  resource its creator could not open.
- Deleting a user must delete or reassign their resources. An orphaned
  resource keeps a valid `organization_id` but has no owner, so it stays
  inside the boundary while belonging to nobody.
- **Conversations do not follow the rule model**, so a future resource type
  copied from them will be private and one copied from agents will be
  rule-governed. If conversations ever need to be shareable — support access,
  audit, a team view — that is a new decision, not a bug to fix by adding the
  rules back: the handlers would need `RequireResource` alongside the owner
  comparison, and the owner comparison would have to stay as the default that
  ownership expresses.

