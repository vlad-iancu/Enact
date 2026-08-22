# Connecting accounts

Some tools act on your accounts elsewhere — reading your mail, searching your
Jira, opening a document in your Drive. For that to work you connect the
account once, and the platform holds the credential on your behalf.

## Providers and identities

A **provider** is the third party as your organization configured it —
"google", "atlassian", "github". An owner registers it once, with your
organization's own OAuth client where one is needed, so consent screens carry
your organization's name and quotas land on its account.

An **identity** is your personal credential for one provider. It belongs to
you and nobody else: an agent shared with a colleague uses *their* identity
when they run it, never yours.

Two kinds exist:

- **OAuth** — you are sent to the provider, you approve, and the platform
  stores the resulting token and refreshes it before it expires.
- **Token** — you paste a token or API key the provider issued you.

## Access levels

A provider declares named **access levels** — "read", "readwrite" — each
standing for the concrete scopes it needs. Tools ask for a level rather than
a list of scopes, so what you are agreeing to stays legible.

Connecting again at a higher level adds to what you already granted rather
than replacing it, so approving "readwrite" never quietly removes access
something else was relying on.

## When a tool needs an account you have not connected

The tool call **fails immediately** and says what to connect. Nothing waits
for you in the background — connect the account and ask again.

You can avoid the round trip: an agent's page lists every credential its tools
need and whether you have it, so you can connect before starting rather than
finding out mid-conversation.

## Disconnecting

Disconnecting revokes the credential at the provider where the provider
supports it, then deletes it here. Every agent and MCP server that relied on
it loses access at once.

If an owner deletes the **provider** itself, every user's credential for it is
revoked and deleted with it — a stored credential whose provider is gone could
be neither used nor revoked, so it is never left behind.
