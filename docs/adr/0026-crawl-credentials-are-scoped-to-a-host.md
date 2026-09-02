# ADR-0026: Crawl credentials are sealed, unreadable, and scoped to a host

**Date**: 2026-09-01
**Status**: accepted
**Deciders**: platform owner, Claude

## Context

Extraction rules made a JIRA ticket readable once fetched, but the fetcher
sends no credentials, so a ticket URL returns the login page and the selectors
match nothing. Crawling anything behind authentication needs the platform to
hold a secret and present it.

That is a different kind of risk from anything else the crawler does. The
platform would be storing somebody's token and sending it outward, from its own
network position, to a site chosen by a link it followed.

## Decision

A crawl carries **credential rules**: a URL pattern and a set of request
headers, presented only to URLs matching the pattern.

- The pattern's **host must be concrete**. `https://jira.example.com/*` and
  `https://*.example.com/` are accepted; `https://*` and `*/browse/*` are
  refused at validation.
- Values are sealed with AES-256-GCM under `CRAWL_ENCRYPTION_KEY`, a **separate
  key** from the identity service's.
- There is **no read path**. Redaction is a method on `Crawl` applied at every
  response site, so header names and hosts are visible and values never are.
- Headers are applied in a `RoundTripper`, re-evaluated per redirect hop.
- `Host`, `Content-Length`, the hop-by-hop headers, `User-Agent` and `From`
  cannot be set.

## Alternatives considered

### Alternative 1: Credentials on the crawl, not scoped to a pattern
- **Pros**: simpler to configure — one token per crawl.
- **Cons**: a crawl follows links and links leave sites. The token would be
  presented to whatever the crawl wandered onto, which for a JIRA token means
  handing it to a third party because somebody put a link in a ticket.
- **Why not**: the scoping *is* the security property.

### Alternative 2: Reuse `IDENTITIES_ENCRYPTION_KEY`
- **Pros**: one key to manage.
- **Cons**: a crawl header and an OAuth refresh token are held for different
  reasons with different blast radii.
- **Why not**: one leaked key should not open both. The vault moved to
  `internal/secrets` so both domains share the code without sharing the key.

### Alternative 3: Set the headers on the outgoing request
- **Pros**: obvious, three lines.
- **Cons**: `http.Client` copies headers onto redirect hops. It strips
  `Authorization`, `Cookie` and `WWW-Authenticate` only across a different
  *domain*, and a custom `X-Api-Token` never. **Measured with a plain client,
  both headers reached the redirect target** — different ports of one host are
  not a domain change.
- **Why not**: it leaks, provably.

## Consequences

### Positive
- Verified live: a wildcard host is a 400, `User-Agent` is a 400, a stored
  token is `v1.<fingerprint>.<ciphertext>` with no plaintext in the index, and
  the API returns `{"Authorization": ""}`.
- An update that omits the headers leaves them in place, so a query can be
  edited without handling secrets.
- The same machinery seals the JIRA API token — one `SealCrawl`, so a new kind
  of secret cannot be added without passing through it.

### Negative
- **Both crawl services now fail to start without `CRAWL_ENCRYPTION_KEY`.**
  Deliberate — a silent plaintext fallback is worse — but any environment needs
  the key before deploying.
- Header auth only. A site needing a username and password typed into a form is
  still unreachable, and an expiring token must be replaced by hand.

### Risks
- A user can still point a credentialed crawl at a host they do not control, if
  they write that host into the pattern. The platform enforces that the secret
  goes only where the pattern says; it cannot know whether the pattern is
  right.
- Key rotation is supported by the vault (`CRAWL_ENCRYPTION_KEYS_OLD`) but
  nothing re-seals lazily yet, so a retired key must stay listed.
