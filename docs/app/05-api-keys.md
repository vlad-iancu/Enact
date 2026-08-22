# API keys

An **API key** lets a program act as you. It is what a script, a scheduled
job, or a workflow tool like n8n uses in place of signing in.

## Creating one

From your profile: give the key a name, and it is shown **once**.

Copy it then. It cannot be retrieved later — the platform stores only a
one-way hash of it, so there is nothing to show you afterwards and nothing
useful in a stolen backup. If you lose a key, revoke it and make another.

Keys look like this:

```
enact_sk_LZPmXIq7v9Kd3fRb2wNxT4hJ8sYc6mAe1QgZ0pUv
```

Your key list shows only each key's name and first few characters — enough to
tell them apart when deciding which to revoke.

## Using one

Send it as a bearer token:

```bash
curl https://your-enact-host/agents \
  -H "Authorization: Bearer enact_sk_…"
```

or, equivalently, in a dedicated header:

```bash
curl https://your-enact-host/agents \
  -H "X-Enact-Api-Key: enact_sk_…"
```

Both work. Use whichever your client makes easier.

```bash
curl -N -X POST https://your-enact-host/inference \
  -H "Authorization: Bearer enact_sk_…" \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"…","messages":[{"role":"user","content":"Hello"}]}'
```

The reply streams back as server-sent events, exactly as it does in the app.

## What a key can do

**A key is you.** It carries your permissions — no more, and no less. If a
role of yours is revoked, every key of yours loses that access at the same
moment. If an agent is not yours to run, your key cannot run it either.

It can reach:

- **Your profile** — `GET /auth/me` answers for a key, which is how a program
  checks that a key is still live and finds out whose it is
- **Agents** — create, edit, delete, upload RAG documents
- **Knowledge bases** — the same
- **MCP servers** — register, manage, and call their tools
- **Inference** — with an agent or with a bare model
- **Conversations** and the **model list**

It cannot reach:

- **Organizations and members** — who belongs where, and who may do what
- **Connected accounts and providers** — connecting an account is a consent
  step that has to involve a person
- **Administration**
- **API keys themselves** — a key cannot create or revoke keys, including
  itself. Reading your profile is fine; changing your credentials is not. Otherwise revoking a stolen key would achieve nothing: whoever took
  it could simply issue a replacement first.

Those four remain available when you are signed in.

## Things worth knowing

- **A key does not expire.** Unlike signing in, it keeps working until you
  revoke it. Treat one like a password: store it in your automation tool's
  secret store, never in a repository or a shared document.
- **Revocation is immediate.**
- **Tools still act as you.** An agent that reads your email through a key
  reads *your* mailbox, using the accounts *you* connected. A key does not
  grant access to anyone else's.
- **Use one key per purpose.** Separate keys for separate jobs mean revoking
  one does not stop the others, and the "last used" timestamp tells you which
  are still doing something.
