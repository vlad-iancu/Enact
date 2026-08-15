# ADR-0018: Conversations record tool calls and replay them into the model's context

**Date**: 2026-08-15
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru

## Context

Bedrock Converse is stateless: every request carries the whole conversation,
which this platform rebuilds from storage each turn. A stored message was
`{role, content}` and nothing else, so a turn that called three MCP tools was
persisted as the sentence it ended with.

Two things were lost by that. Reopening a thread showed an answer with no
trace of the tools that produced it, so a UI could render tool activity live
and never again. And the model itself lost the exchange: on the next turn it
saw its own conclusion but not what it had searched for or received, so it
could not answer "what did you look at?" and could re-run a tool it had
already run.

Worse, the assistant's text was accumulated across the whole turn into one
string, so "let me check that" and "the answer is 42" were stored as one
sentence that was never said.

## Decision

An assistant message carries `tool_calls`: for each call the server, tool,
`tool_use_id`, the **model's** arguments, the result as the tool returned it
(`content` and `structured_content`), `is_error`, the tool-loop round
(`turn`), and the assistant's words in that round (`text`).

That record is replayed into the model's context on later turns, rebuilding
the rounds as they happened:

```
assistant { text, tool_use }    round 1
user      { tool_result }
assistant { text, tool_use }    round 2 — made KNOWING round 1's results
user      { tool_result }
assistant { text }              the answer
```

`message.content` is therefore the final round's text only. Structured
results are replayed as native `ToolResultContentBlockMemberJson` blocks.
`tool_calls` is stored but **not indexed**.

## Alternatives Considered

### Alternative 1: store tool calls, do not replay them
- **Pros**: the UI gets everything it needs; conversations stay cheap,
  because history remains text.
- **Cons**: the model's view and the user's view of the same conversation
  diverge — the user can see the tool call on screen and the assistant
  cannot.
- **Why not**: it fixes the display and leaves the reasoning broken. A
  follow-up question about a previous tool call is exactly the case that
  motivated recording them.

### Alternative 2: replay tool calls flattened into one round
- **Pros**: simpler storage — no `turn`, no per-round text.
- **Cons**: a second round's call was made *knowing* the first round's
  results. Presenting both as one parallel request tells the model something
  untrue about its own reasoning.
- **Why not**: a record that misrepresents the exchange is worse than one
  that omits it.

### Alternative 3: truncate large tool results before replaying
- **Pros**: bounds the token cost, which is otherwise unbounded and
  compounds every turn.
- **Why not**: the model would "remember" a result it never saw. If a cap is
  ever needed it must be visible in the record, not silent.

### Alternative 4: index `tool_calls`
- **Why not**: a tool's arguments are arbitrary model JSON. Under a
  dynamically-mapped object every argument name becomes an index field, so a
  single chatty tool could blow up the mapping. Nothing queries them.

## Consequences

### Positive
- Reopening a conversation reproduces the turn, including what each tool was
  asked and what it answered.
- The model keeps its own working memory across turns and can avoid redoing
  work.
- `message.content` is now the answer rather than a concatenation of
  everything said during the turn.
- Structured results reach the model as JSON, not as a string that happens to
  look like JSON.

### Negative
- **Tool results are re-sent, and re-charged as input tokens, on every
  subsequent turn** for the life of the conversation. A tool returning
  something large compounds. There is no truncation, by choice.
- Conversation documents grow with tool output.
- Bedrock validates the pairing strictly: a `tool_use` with no matching
  `tool_result` in the very next message is rejected, so both halves are
  built from one record and dropped together. A call whose tool the agent no
  longer has is skipped entirely, and only its text survives.

### Risks
- The arguments stored are the model's, *before* credential injection — a
  deliberate infidelity, so a stored conversation can never carry a user's
  token. A replay therefore shows arguments slightly different from what was
  sent.
- Nothing records what the *tool* saw: headers, the server reached, timing.
  The record reproduces the model's view of the exchange, not the
  platform's.
