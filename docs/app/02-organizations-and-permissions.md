# Organizations and permissions

Every user belongs to exactly one **organization**, every resource belongs to
exactly one, and nothing crosses between them.

A brand-new account belongs to none. It can sign in, see its profile, and ask
for an organization — everything else is refused until an administrator
approves the request. The person who asked becomes the organization's first
**owner**.

## Permissions

A permission is four parts separated by colons:

```
enact:<resource-type>:<action>:<resource-id>

enact:kb:edit:9f2a…      one knowledge base
enact:agent:view:*       every agent
enact:kb:*               every action on every knowledge base
```

`*` stands in for one part, and a rule can stop early to cover everything
below it. `resource-type` is one of `kb`, `agent`, `mcp-server`, `provider`,
`identity`, `conversation`, `user`, `role`, `organization`. `action` is one
of `view`, `edit`, `delete`, `create`, `use`.

**`use` is the one that is not record-keeping**: running an agent, retrieving
from a knowledge base, calling a server's tools, connecting an account through
a provider. Being able to *see* an agent does not mean being able to *run* it,
which is usually the distinction people are looking for.

## Roles

Rules are grouped into **roles**, and roles live inside an organization. An
owner defines them and assigns people to them.

You do not need a role for the things you create: creating a resource makes
you its owner, which grants you every action on that resource and nothing
else. Roles are for sharing — `enact:agent:view:*` lets someone see the
organization's agents, `enact:kb:*` hands over the knowledge bases entirely.

`GET /organizations/me` reports your organization, whether you own it, and
the rules you hold; `/organizations/me/roles` shows which roles they came
from.

## Owners

An owner may do anything inside their own organization, without a rule saying
so — and nothing at all outside it. They are the only people who can define
roles, assign them, add members, and create accounts within the organization.

## What you will see when something is refused

A resource you may not reach reads as **not found**, not "forbidden" — "not
yours" and "does not exist" are deliberately indistinguishable, so ids cannot
be probed for existence.

The exception is having no organization at all, which says so plainly, because
that is the one thing you can act on: ask for one.

## Conversations are private

Conversations are yours alone. No role grants access to somebody else's, and
an owner cannot read them either. They hold your words and the full output of
every tool run on your behalf, which is a different thing from an
organization's shared resources.
