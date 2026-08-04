# ADR-0006: Queue consumers reclaim stalled deliveries and cap attempts

**Date**: 2026-08-02
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

The document-indexing queue (Redis Streams consumer group) acked messages
only after successful processing, but consumers only ever read *new*
messages. A delivery abandoned by a dead consumer stayed pending forever —
observed in production-like use: two documents stranded for three weeks,
silently leaving a knowledge base empty.

## Decision

`queue.Consumer` sweeps the group's pending list on startup and every
`REDIS_RECLAIM_INTERVAL` (30s): deliveries idle beyond
`REDIS_RECLAIM_MIN_IDLE` (2m) are claimed via XPENDING/XCLAIM and retried;
messages that reach `REDIS_MAX_DELIVERIES` (5) attempts are acked away and
surfaced through a `Dropped` callback (the indexer logs them). The queue
package itself does no logging.

## Alternatives Considered

### Alternative 1: Retry only on consumer restart (XREADGROUP from "0")
- **Pros**: minimal code
- **Cons**: only recovers own-consumer messages; a dead consumer's pending list stays orphaned; no poison handling
- **Why not**: the observed failure was precisely an orphaned dead-consumer delivery

### Alternative 2: Dead-letter stream for exhausted messages
- **Pros**: failed payloads remain inspectable/replayable
- **Cons**: more moving parts; nothing consumes the DLQ today
- **Why not**: at current scale a logged drop (with message id) is enough; a DLQ can supersede this ADR later

## Consequences

### Positive
- Work survives consumer crashes; poison messages cannot clog the queue
- Behavior is env-tunable per deployment

### Negative
- At-least-once semantics become visible: a slow (> min-idle) but alive consumer's message can be processed twice; document indexing is idempotent (indexed by document id), which makes this safe today

### Risks
- Handlers added later must stay idempotent or deduplicate — noted here as the standing requirement