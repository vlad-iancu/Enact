# Tools and MCP servers

An agent's tools come from **MCP servers** — services that expose a set of
callable tools. The platform ships some, and your organization can register
any other that speaks the protocol.

## Built-in servers

The platform hosts these itself; pick one from the list rather than typing an
address. **Google Workspace** covers Gmail, Calendar, Drive, Docs, Sheets and
Slides, and acts as whoever runs it using their own connected Google account.

## Registering your own

A server needs an id, its URL, and its transport (`streamable-http` or `sse`).
The id is yours to choose and only has to be unique **within your
organization** — two organizations can each register a "github" without
colliding.

Registration is not a formality: the platform connects to the server, performs
the MCP handshake and reads its tool list. A server that cannot be reached, or
that refuses the handshake, is refused rather than stored broken. The tool list
is cached and refreshed periodically, so tools added upstream appear without
re-registering.

## Telling a server which credential to use

If a server's tools need one of your accounts, say so per tool:

- **`tool_access_requirements`** — which provider and access level each tool
  needs. This is what lets the platform refuse a call up front, naming the
  account to connect, instead of letting it fail at the far end.
- **`tool_authorizations`** — how the credential is presented: usually an
  `Authorization` header built from a template.

Two special keys cover the handshake itself: **`/initialize`** and
**`/list-tools`**. Servers that refuse an anonymous handshake — Atlassian's
does — need credentials at registration too, and those keys are how the
platform knows which to present. `*` applies an entry to every tool.

## What tools can do

Tools declare whether they only read or may change things, and destructive
ones say so. A tool acts with the permissions of the credential behind it —
a read-only token yields read-only tools, however the tool is described.

Tool calls and their results are recorded in the conversation, so what an
agent did on your behalf is visible afterwards rather than inferred from
its answer.
