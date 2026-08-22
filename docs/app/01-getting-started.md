# Getting started

enact runs **agents**: a model, a system prompt, and whatever you give it to
work with — knowledge bases to read from, and MCP servers whose tools it can
call on your behalf.

## Your first conversation

1. **Create an agent.** A name, a model and a system prompt are enough. You
   can attach knowledge bases and tools later without starting again.
2. **Start a conversation** and send it a message. The reply streams back as
   it is generated.
3. **Watch what it does.** When an agent uses a tool, the conversation shows
   the call and its result — not just the conclusion. Reopening the thread
   later shows the same thing, because tool calls are stored with the
   messages.

## What an agent can reach

Nothing by default. An agent has access to exactly what you attach:

- **Knowledge bases** — documents you upload. The agent retrieves the
  passages relevant to each question rather than reading everything.
- **MCP servers** — tools it can call. Some need an account of yours
  connected first; see [Connecting accounts](connecting-accounts).

An agent acts as **the person talking to it**, not as whoever created it. If
a colleague uses your agent to search email, it searches *their* mailbox with
*their* credentials, and only if they have connected that account themselves.

## Replies as data, not prose

An agent can be given an **output schema** — a [JSON Schema](https://json-schema.org)
its replies must match. Instead of a paragraph, you get JSON:

```json
{
  "type": "object",
  "properties": {
    "sentiment": { "type": "string", "enum": ["positive", "negative"] },
    "score": { "type": "number" }
  },
  "required": ["sentiment", "score"]
}
```

asked "I absolutely loved this film" answers
`{"sentiment":"positive","score":0.97}` — every time, in that shape.

This is for agents something else consumes: a classifier, an extraction step,
anything whose output another program parses. For an agent people chat with,
leave it unset.

Notes worth knowing:

- The schema can be set when you create the agent, and changed afterwards.
  Clearing it returns the agent to ordinary prose.
- It works alongside tools. On a turn where the agent calls a tool there is no
  prose to constrain; the schema applies to the answer it finally gives.
- Streaming still streams — the JSON arrives in pieces, so wait for the end
  before parsing.
- If the model cannot satisfy the schema, the request fails rather than
  returning prose you were about to parse as JSON. A schema that asks for
  something the model cannot know is the usual cause.

## Where things live

Everything you create belongs to your **organization** — agents, knowledge
bases, MCP servers and connected accounts. Nothing crosses between
organizations, and what you can see inside yours depends on the roles you
hold. See [Organizations and permissions](organizations-and-permissions).
