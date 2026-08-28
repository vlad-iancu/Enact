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

- **Knowledge bases** — documents you upload. See below for the two kinds.
- **MCP servers** — tools it can call. Some need an account of yours
  connected first; see [Connecting accounts](connecting-accounts).

An agent acts as **the person talking to it**, not as whoever created it. If
a colleague uses your agent to search email, it searches *their* mailbox with
*their* credentials, and only if they have connected that account themselves.

## Two kinds of knowledge base

You choose the kind when you create a knowledge base, and it cannot be changed
afterwards — the documents are stored differently, so switching would leave
what is already there in a form nothing reads.

- **Context** — every document is read **whole**, every time. Good for
  material that is always relevant and small enough to fit: a style guide, a
  policy, a glossary. An agent can reference as many of these as you like.
- **Retrieval** — documents are split into passages and indexed by meaning.
  The agent gets only the passages relevant to the question asked. Good for
  large corpora: a handbook, an archive, years of notes. An agent can attach
  **one**.

A retrieval knowledge base can also say **how** it splits documents, with a
chunk size and an overlap measured in characters (1000 and 150 by default).
Smaller chunks retrieve more precisely but give the model less surrounding
context; larger ones do the reverse. Overlap keeps a sentence that straddles a
boundary from being lost to both sides. Like the kind, these are set when you
create the knowledge base and cannot be changed — documents are split as they
arrive, so a later change would leave one knowledge base holding two
differently-shaped sets of passages. If you want different chunking, create a
new knowledge base and upload again.

One retrieval knowledge base, not several, because relevance is only
comparable inside a single collection — searching two and merging the results
tends to let one crowd the other out for reasons that have nothing to do with
the question. If an agent needs two sources, put them in the same knowledge
base.

A knowledge base is its own thing, not part of an agent: several agents can
use the same one, sharing it is a matter of permissions
([Organizations and permissions](organizations-and-permissions)), and deleting
an agent leaves it untouched.

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
  "required": ["sentiment", "score"],
  "additionalProperties": false
}
```

asked "I absolutely loved this film" answers
`{"sentiment":"positive","score":0.97}` — every time, in that shape.

**Every object in the schema must set `"additionalProperties": false`** — the
one at the top, any nested object, and objects inside arrays. That is a rule of
the underlying model API, not of enact, and a schema missing it is rejected
when you save the agent, with the offending object named.

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
