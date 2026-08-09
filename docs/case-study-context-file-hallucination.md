# Case study: hallucinated answers about a removed context file

**Date observed**: 2026-08-08
**Area**: conversations / ad-hoc context files (Bedrock DocumentBlocks)
**Status**: recorded; behavior is inherent to LLM chat, mitigations listed below

## What happened

A user attached a multi-task report (`report.pdf`) to a conversation message
and asked about task 1 — answered correctly, grounded in the file. On a
**later turn, without the file attached**, they asked about task 2. The model
produced a detailed, confident, correctly-formatted answer — which turned out
to be **fabricated**.

## The evidence

Two conversations in `enact-conversations` captured both sides (both
model-only `claude-haiku` — no agent, no KB, so no hidden grounding):

- Conversation `7214e82a`, task 2 asked **with** the file:
  *"Task 2 Implementation — **Temporal Localization** — finds where a …"*
  — matching the actual report.
- Conversation `0b9c9ffb`, task 2 asked **without** the file:
  *"Task 2 involves **tracking multiple vehicles across both cameras**"*,
  complete with a plausible pipeline (ByteTrack + YOLOv8, 5-frame filter,
  track repair) — **contradicting the real report**.

The fabricated answer is structurally a clone of the task-1 reply that sat
in the transcript: same headings, same building blocks (ByteTrack, IoU ≥ 0.3
chaining, interpolation, cameras A/B), even the phrase "Similar to Task 1".

## Why it happens

1. **Context files are one-turn context, by design.** The file's bytes go to
   the model (as a Converse DocumentBlock) only on the turn that carries
   them; later turns replay the message history without re-sending the file
   (see `internal/enactmain/conversations.go` — `addMessage` and the
   `Attachments` field's doc comment). Re-sending would re-bill the file's
   tokens on every message.
2. **History carries only what was *said*.** The model's grounded replies
   (and the user's questions) persist as text; the model "remembers" the
   file exactly to the extent its content leaked into the transcript.
3. **Models fill gaps instead of declining.** Asked about task 2 — never
   discussed — the model did not say "the file is no longer attached". It
   extrapolated from the transcript's technical vocabulary plus prior
   knowledge of what such a project plausibly contains, and presented the
   result with the same authority as the grounded answers around it.

The failure is *particularly* deceptive because the hallucination inherits
the formatting and terminology of genuine file-grounded answers in the same
conversation.

## How to distinguish grounded from fabricated

- Each stored user message records `attachments` (filenames only). An
  assistant reply is file-grounded only if the *user message it answers*
  carried the attachment — or the question is answerable from earlier
  transcript text.
- Cross-check against a turn where the file WAS attached (as done above), or
  against KB-grounded answers.

## Mitigations

- **UI legibility** (recommended): render the paperclip on exactly the
  messages that carried attachments; optionally hint in the composer that
  previously attached files are no longer sent.
- **Use persistent grounding for documents interrogated across turns**: a
  knowledge base (whole document, every turn) or the agent's RAG collection.
  Ad-hoc attachments are for questions about a file *now*.
- **Optional guardrail** (not implemented): a platform system-prompt line for
  conversations — "if asked about an attached file's content not present in
  this conversation, say so rather than inferring" — reduces but does not
  eliminate the failure mode.

## Related

- ADR-0012 (integration tests), ADR-0009 (service domains) — context only.
- Context-file mechanics: `internal/enactmodelinference/handler.go`
  (`convertContextFiles`), `internal/bedrock/client.go` (DocumentBlocks),
  `internal/enactmain/conversations.go` (pass-through + persistence).