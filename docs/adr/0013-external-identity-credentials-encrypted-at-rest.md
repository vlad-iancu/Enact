# ADR-0013: External identity credentials are encrypted at rest with an application key

**Date**: 2026-08-12
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru

## Context

Agents need to act at third parties on their user's behalf: the GitHub MCP
server expects `Authorization: Bearer ghp_…`, Google Workspace and JIRA
expect OAuth access tokens. The platform therefore has to hold long-lived
credentials belonging to real people — a materially different asset from
anything it stored before.

Every other secret-shaped value on the platform is either configuration
(Bedrock keys, the OAuth client secret — operator-owned, in env files) or
one-way (`password_hash` via bcrypt, `verification_token_hash` via SHA-256).
Neither pattern fits: these credentials must be replayed verbatim to the
provider, so hashing is impossible, and they belong to users rather than to
the operator.

Stored as plaintext in OpenSearch, a cluster snapshot, a stray backup, or a
misconfigured Dashboards instance would expose every user's GitHub and
Google access at once.

## Decision

The `enact-external-identities` service seals every credential with
**AES-256-GCM** before it reaches OpenSearch, using a 32-byte key supplied
as `IDENTITIES_ENCRYPTION_KEY` (base64). Sealing and opening happen in
exactly one place — the domain repository — so no provider implementation
and no handler ever touches the key.

Sealed values are `v1.<key-fingerprint>.<base64url(nonce||ciphertext)>`. The
fingerprint (first 8 hex of SHA-256 of the key) names *which* key sealed a
value without revealing it, so a botched rotation is diagnosable and
`IDENTITIES_ENCRYPTION_KEYS_OLD` can hold decrypt-only predecessors.

A missing or malformed key is a **startup failure**. There is no plaintext
fallback.

## Alternatives Considered

### Alternative 1: Plaintext with a non-indexed field
- **Pros**: zero code; consistent with the platform's current posture, where
  AWS keys already sit in env files on the same host.
- **Cons**: anyone who can read the cluster — an operator, a snapshot, a
  Dashboards session, a future misconfigured proxy — reads every user's
  third-party access.
- **Why not**: the blast radius is other people's accounts at other
  companies, not this platform's own data. That asymmetry justifies code the
  rest of the platform does not have.

### Alternative 2: AWS KMS envelope encryption
- **Pros**: the key never exists in the application's memory or env; access
  is auditable in CloudTrail; rotation is managed.
- **Cons**: a KMS call on every read and write, an IAM dependency in the
  hot path, cost per operation, and a hard dependency on AWS for a service
  that otherwise only needs OpenSearch.
- **Why not**: deferred, not rejected. The `v1.` prefix and the fingerprint
  exist so a `v2.` KMS format can be introduced without migrating stored
  values first.

### Alternative 3: Hash-only, like verification tokens
- **Why not**: impossible. A credential that cannot be replayed is not a
  credential.

## Consequences

### Positive
- A leaked OpenSearch snapshot yields ciphertext.
- One choke point means no provider can forget to encrypt, and providers
  stay unit-testable without key material.
- Rotation is possible without downtime (seal with the new key, open with
  either), and the fingerprint makes the migration state observable.
- Fail-closed startup means a misconfigured deployment is loud, not quietly
  insecure.

### Positive (lifecycle, added later)
- **Deleting a credential ends it.** `Provider.Revoke` is part of the
  interface, so every credential type must answer the question "how is this
  invalidated at the source?" — OAuth with RFC 7009 against the record's
  `revoke_url`, PAT with an explicit `ErrRevocationUnsupported` rather than
  silence. All three deletion paths (disconnect, account deletion, forced
  provider deletion) go through one `revokeAll`, so "deleted" cannot come to
  mean "left live at the provider" in one of them. The forced provider
  deletion revokes before the cascade: afterwards the record holding the
  revocation endpoint and client secret is gone, and nothing could end those
  grants.
- **Replacing a credential does not revoke it.** Most providers treat
  revocation as ending the whole grant, and a re-authorization frequently
  returns the same refresh token, so revoking on overwrite would destroy the
  credential just obtained. Revocation belongs to deletion only.
- **Deleting an account deletes its credentials** (`DELETE
  /v1/identities/all`, called by `adminDeleteUser` before the account
  record). Credentials must not outlive the account that authorized them.

### Negative
- **Losing the key loses every credential.** There is no recovery path but
  re-consent by every user. The key must be backed up out of band the day
  it is generated.
- **Revocation is best effort, and deliberately so.** A provider outage, a
  missing `revoke_url` or an unopenable envelope must not stop a user from
  deleting a credential — the local copy is the part this platform controls,
  and refusing to delete it would be the worse failure. The consequence is
  that "disconnected here" and "revoked there" can diverge; the outcome is
  logged per delete (`revocation=revoked|unsupported|failed`) and counted per
  account deletion, but nothing retries and nothing tells the user.
- Account deletion is the one place where a credential failure *is* fatal:
  it aborts before removing the account, because a half-deleted account
  leaves live grants nothing can map back to a person.
- The encrypted field cannot be searched or aggregated. Everything the
  platform queries on — `expires_at`, `refreshable`, `status`,
  `provider_type` — is deliberately lifted out of the envelope into plain
  indexed fields, which means those few facts about a credential *are*
  visible to anyone reading the cluster.
- A key rotation leaves values sealed with the old key until something
  re-seals them; no such tool exists yet.

### Risks
- **Key handling.** The key lives in `deploy/app.env` alongside AWS
  credentials, so the encryption is only as strong as host access control.
  KMS (alternative 2) is the intended answer once this is more than a
  development deployment.
- **`X-User-Id` is impersonation, not authentication** (see
  `internal/identity`). Any service — or anyone who can reach the service
  directly — can request any user's credential by naming them. Mitigated
  today by keeping every read route service-only in the ACL and having
  enact-main scope reads to the session; this is the natural first place to
  require a real caller assertion if the platform ever grows one.
- **Provider deletion takes its identities with it**, revoking each at the
  provider first. An identity whose provider is gone is unopenable — the
  record is what parses the envelope — and unrevokable, because revocation
  needs that same record's endpoint and client secret. Refusing the delete
  while identities existed (the earlier design, with a `force=true` escape)
  only moved the cleanup to whoever deleted them by hand, and a half-done
  cleanup left exactly the live grants this is meant to prevent. Should an
  orphan exist anyway, retrieval answers **424** naming the missing provider
  — deliberately not 409, which in this API means "the user must
  re-authorize".
