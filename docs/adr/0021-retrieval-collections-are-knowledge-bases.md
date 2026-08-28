# ADR-0021: Retrieval collections are knowledge bases, and an agent attaches one

**Date**: 2026-08-26
**Status**: accepted
**Deciders**: platform owner, Claude

## Context

Until now the platform had two unrelated document stores. **Knowledge bases**
were first-class resources — created through `enact-kb-api`, owned, shared and
permissioned under `enact:kb:…` — whose documents were loaded whole into a
model's context. **RAG collections** were not resources at all: chunks keyed by
`agent_id` (and the uploader's `user_id`), uploaded through
`POST /v1/agents/{id}/rag/documents`, reachable only by the agent that owned
them.

That asymmetry cost more than it looks. A corpus could not be shared between
two agents without uploading, chunking and embedding it twice. It could not be
handed to a colleague, because there was no resource to grant. It was destroyed
when the agent was deleted, which is not what "delete this assistant" should
mean. And a chunk's `user_id` filter meant that even inside one agent, a
document uploaded by one person was invisible to another — authorization
implemented by accident, in the wrong layer, in a way nobody had chosen.

## Decision

A retrieval collection **is** a knowledge base. A knowledge base gains a
`kind`: `context` (documents stored whole, the previous behaviour and the
default) or `rag` (documents chunked and embedded). An agent references any
number of context knowledge bases through `knowledge_base_ids`, and **exactly
one** retrieval knowledge base through `rag_knowledge_base_id`.

Chunks are re-keyed from `agent_id` to `kb_id`, and the `user_id` filter is
dropped: a knowledge base is shared through RBAC rules, so filtering by the
reader's own id would hide a colleague's documents in a knowledge base they
were given. The agent API's three RAG document routes are removed; documents of
either kind go through `POST /v1/knowledge-bases/{id}/documents`.

## Alternatives Considered

### Alternative 1: Leave RAG on the agent, add sharing later
- **Pros**: no migration; no change to any existing client.
- **Cons**: sharing would have to reinvent ownership, grants and org scoping
  for a second resource type that is not one.
- **Why not**: the feature asked for is exactly the one knowledge bases already
  have. Building it twice means maintaining it twice, and the second copy is
  the one that drifts.

### Alternative 2: A separate "retrieval collection" resource, `enact:collection:…`
- **Pros**: a clean type, no `kind` discriminator, no default to get right.
- **Cons**: a third resource type in RBAC, a third set of routes, a third UI
  surface — for something users would describe as "a knowledge base I upload
  documents to".
- **Why not**: the difference between the two is *how the documents are read at
  inference time*, not what they are. That is a property of the knowledge base,
  which is what `kind` says.

### Alternative 3: Allow an agent several retrieval knowledge bases
- **Pros**: no artificial ceiling; combining corpora needs no re-upload.
- **Cons**: k-NN ranks passages by distance within one collection. Searching
  several and merging their scores compares numbers that were never comparable;
  the usual outcome is one corpus quietly crowding out another for reasons
  unrelated to the question.
- **Why not**: the merge has no correct implementation without cross-collection
  calibration the platform does not have. Combining sources by putting them in
  one knowledge base — where they are embedded the same way — gives a ranking
  that means something. Context knowledge bases keep no such limit, because
  nothing about them is ranked.

## Consequences

### Positive
- One corpus, many agents: embed once, attach anywhere.
- Sharing a corpus is a grant, using machinery that already exists and is
  already tested.
- Deleting an agent no longer destroys documents. Deleting the knowledge base
  does, which is where that decision belongs.
- Uploads, listings and deletions have one shape for both kinds, so the KB API
  is the single answer to "where do documents go".

### Negative
- A breaking API change: `POST`/`GET`/`DELETE /v1/agents/{id}/rag/documents`
  are gone, with no compatibility shim. They are replaced by the knowledge-base
  routes, which have the same shapes.
- Existing installations need a migration (`scripts/migrate-rag-knowledge-bases`)
  to turn each agent's collection into a knowledge base, grant it to the
  agent's owner, and re-key the chunks. Until it runs, those agents retrieve
  nothing.
- `kind` is fixed at creation. Changing it would leave existing documents
  stored in a form nothing reads, which is worse than refusing.

### Risks
- **Putting a knowledge base in the wrong field** fails silently in both
  directions. A context KB in `rag_knowledge_base_id` has no embeddings to
  search, so retrieval returns nothing — indistinguishable from "no relevant
  passage". A retrieval KB in `knowledge_base_ids` lists documents that exist
  only as chunks, so the listing carries no text and the prompt gets a named
  but *empty* file: the model is told it has a document and handed nothing.
  Both are mitigated by validating the kind at save time, where the error names
  the offending knowledge base and the field it belongs in.
- **The index name `enact-agent-rag-chunks` now lies**: it holds knowledge-base
  chunks. Kept deliberately so existing deployments' data stays where it is;
  renaming it would buy tidiness for the price of a reindex.
- **A KB record with no kind** (created before this) is read as `context`,
  which is what it was. The alternative — refusing to read it — would break
  every existing knowledge base for a field nobody had the chance to set.
