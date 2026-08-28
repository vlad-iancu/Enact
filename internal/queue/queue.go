// Package queue implements the document-indexing work queue on top of Redis
// Streams. enact-kb-api publishes DocumentMessages via a Producer; the
// enact-kb-document-indexer consumes them through a Consumer using a consumer
// group so work is distributed and acknowledged reliably.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// scopeName is the instrumentation scope for the consumer spans created when
// dispatching messages; it identifies this package in Tempo.
const scopeName = "enact/internal/queue"

// Config holds the environment-driven Redis Streams settings.
type Config struct {
	Addr     string `env:"REDIS_ADDR, default=localhost:6379"`
	Password string `env:"REDIS_PASSWORD"`
	Stream   string `env:"REDIS_STREAM, default=enact-kb-documents"`
	Group    string `env:"REDIS_GROUP, default=indexers"`
	// Consumer names this consumer within the group. Defaults to a generated
	// value when empty.
	Consumer string `env:"REDIS_CONSUMER"`

	// ReclaimInterval is how often the consumer scans the group's pending
	// list for stalled messages — deliveries that were never acknowledged,
	// typically because their consumer died mid-processing or the handler
	// failed. Without this scan such messages would stay pending forever,
	// since XREADGROUP only ever returns new messages.
	ReclaimInterval time.Duration `env:"REDIS_RECLAIM_INTERVAL, default=30s"`

	// ReclaimMinIdle is how long a delivery must sit unacknowledged before
	// it may be claimed away from its original consumer. It must comfortably
	// exceed the longest legitimate processing time, or a slow-but-alive
	// consumer's message gets processed twice.
	ReclaimMinIdle time.Duration `env:"REDIS_RECLAIM_MIN_IDLE, default=2m"`

	// MaxDeliveries caps the processing attempts per message. A message
	// that still fails on its last attempt is acknowledged away (and
	// reported via Consumer.Dropped) so one poison message cannot clog the
	// pending list forever.
	MaxDeliveries int64 `env:"REDIS_MAX_DELIVERIES, default=5"`
}

// DocumentType discriminates what an uploaded document is for and therefore
// how the indexer processes it.
type DocumentType string

const (
	// DocumentTypeKBContext is a document uploaded to a knowledge base. The
	// indexer extracts its text and stores it whole; agents referencing the
	// KB load it into the model context at inference time.
	DocumentTypeKBContext DocumentType = "kb_context"
	// DocumentTypeKBRetrieval is a document uploaded to a RETRIEVAL knowledge
	// base. The indexer extracts, chunks and embeds it, and an agent attached
	// to that KB gets the relevant passages at inference time.
	//
	// The value is unchanged from when these collections belonged to an agent,
	// so a message already on the stream when this shipped still dispatches.
	DocumentTypeKBRetrieval DocumentType = "agent_rag"

	// DocumentTypeKBContextDelete asks the indexer to remove one context
	// document from its knowledge base. Content is empty on these messages;
	// KBID and DocumentID identify the target.
	DocumentTypeKBContextDelete DocumentType = "kb_context_delete"
	// DocumentTypeKBRetrievalDelete asks the indexer to remove one document's
	// chunks from a retrieval knowledge base. Content is empty; KBID and
	// DocumentID identify the target.
	DocumentTypeKBRetrievalDelete DocumentType = "agent_rag_delete"
)

// DocumentMessage is the payload pushed onto the queue for each uploaded
// document awaiting indexing.
//
// KBID identifies the target for BOTH kinds now that retrieval collections
// belong to knowledge bases rather than to agents. AgentID remains only so a
// message queued before that change still carries what it carried.
//
// Content carries the document's raw bytes base64-encoded (StdEncoding). The
// bytes are encoded because a message travels as JSON, which requires valid
// UTF-8 strings; binary formats such as PDF would otherwise be corrupted. The
// indexer base64-decodes Content and hands the bytes to Tika for text
// extraction.
type DocumentMessage struct {
	Type   DocumentType `json:"type"`
	UserID string       `json:"user_id"`
	KBID   string       `json:"kb_id,omitempty"`
	// AgentID is legacy: retrieval documents used to be addressed by agent.
	AgentID    string `json:"agent_id,omitempty"`
	DocumentID string `json:"document_id"`
	Filename   string `json:"filename,omitempty"`
	Content    string `json:"content"`
	// ChunkSize and ChunkOverlap carry the knowledge base's chunking to the
	// indexer, in runes, so a document is split the way its KB was created to
	// split it. Zero means the indexer applies its own configured default —
	// the case for context documents (nothing to chunk) and for retrieval
	// knowledge bases that predate this field.
	//
	// Copied onto the message rather than looked up by the indexer: the
	// message is what gets retried, and re-reading the KB later could apply a
	// different setting to a redelivery than to the first attempt.
	ChunkSize    int `json:"chunk_size,omitempty"`
	ChunkOverlap int `json:"chunk_overlap,omitempty"`

	// Trace carries the W3C trace context (traceparent/tracestate/baggage)
	// of the request that published the message, so the consumer's async
	// processing joins the same distributed trace — its logs carry the
	// original trace_id and its span nests under the upload request in
	// Tempo. Populated by Publish; empty on messages from older producers.
	Trace map[string]string `json:"trace,omitempty"`
}

// Producer publishes DocumentMessages onto the stream.
type Producer struct {
	rdb    *redis.Client
	stream string
}

// NewProducer connects to Redis and returns a Producer.
func NewProducer(cfg Config) *Producer {
	return &Producer{
		rdb:    redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password}),
		stream: cfg.Stream,
	}
}

// Publish appends a document message to the stream, stamping it with the
// caller's trace context so the consumer continues the same trace.
func (p *Producer) Publish(ctx context.Context, msg DocumentMessage) error {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) > 0 {
		msg.Trace = carrier
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("queue: marshal message: %w", err)
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		Values: map[string]any{"data": data},
	}).Err()
}

// Close releases the underlying Redis connection.
func (p *Producer) Close() error { return p.rdb.Close() }

// Consumer reads DocumentMessages from the stream using a consumer group.
// Besides new messages it periodically reclaims stalled pending deliveries
// (see Config.ReclaimInterval), so work abandoned by a dead consumer is
// retried instead of being stranded.
type Consumer struct {
	rdb      *redis.Client
	stream   string
	group    string
	consumer string

	reclaimInterval time.Duration
	reclaimMinIdle  time.Duration
	maxDeliveries   int64

	// Dropped, when non-nil, is invoked after a message exhausts its
	// MaxDeliveries attempts and is discarded. The queue package does no
	// logging of its own; set this to surface drops.
	Dropped func(id string, deliveries int64)
}

// NewConsumer connects to Redis and returns a Consumer. A consumer name is
// generated when cfg.Consumer is empty; zero reclaim settings fall back to
// the Config defaults.
func NewConsumer(cfg Config) *Consumer {
	consumer := cfg.Consumer
	if consumer == "" {
		consumer = fmt.Sprintf("indexer-%d", time.Now().UnixNano())
	}
	if cfg.ReclaimInterval <= 0 {
		cfg.ReclaimInterval = 30 * time.Second
	}
	if cfg.ReclaimMinIdle <= 0 {
		cfg.ReclaimMinIdle = 2 * time.Minute
	}
	if cfg.MaxDeliveries <= 0 {
		cfg.MaxDeliveries = 5
	}
	return &Consumer{
		rdb:             redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password}),
		stream:          cfg.Stream,
		group:           cfg.Group,
		consumer:        consumer,
		reclaimInterval: cfg.ReclaimInterval,
		reclaimMinIdle:  cfg.ReclaimMinIdle,
		maxDeliveries:   cfg.MaxDeliveries,
	}
}

// EnsureGroup creates the consumer group (and the stream itself, via
// MkStream) if it does not already exist. An existing group is not an error.
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, c.stream, c.group, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("queue: create group: %w", err)
	}
	return nil
}

// Handler processes a single message. Returning nil acknowledges the message;
// returning an error leaves it unacknowledged (pending) for later retry.
type Handler func(ctx context.Context, msg DocumentMessage) error

// Run consumes the stream until ctx is cancelled, dispatching each message to
// handler and acknowledging successful ones. It blocks for up to five seconds
// per read, so cancellation is observed promptly.
//
// On startup and every ReclaimInterval thereafter it also sweeps the group's
// pending list: stalled deliveries are claimed and retried, and messages that
// have exhausted MaxDeliveries are dropped (see Consumer.Dropped).
func (c *Consumer) Run(ctx context.Context, handler Handler) error {
	return c.run(ctx, func(ctx context.Context, raw []byte) error {
		var msg DocumentMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return errUnprocessable
		}
		return handler(ctx, msg)
	})
}

// rawHandler processes one message's payload. Returning errUnprocessable
// acknowledges it without retrying — a payload that cannot be decoded will
// never decode, and retrying it forever only clogs the pending list.
type rawHandler func(ctx context.Context, raw []byte) error

// errUnprocessable marks a message that must be dropped rather than retried.
var errUnprocessable = errors.New("queue: message cannot be decoded")

// run is the consume loop, shared by every message type on every stream. It
// deals only in raw payloads so that adding a second kind of work — workflow
// executions alongside document indexing — reuses the reclaim, retry and
// tracing behaviour instead of reimplementing it.
func (c *Consumer) run(ctx context.Context, handle rawHandler) error {
	if err := c.EnsureGroup(ctx); err != nil {
		return err
	}
	var nextReclaim time.Time // zero: reclaim on the first iteration
	for {
		if ctx.Err() != nil {
			return nil
		}
		if now := time.Now(); now.After(nextReclaim) {
			// Best-effort: a failed sweep is retried next tick, and hard
			// connectivity problems surface through XReadGroup below.
			c.reclaimPending(ctx, handle)
			nextReclaim = now.Add(c.reclaimInterval)
		}
		streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.group,
			Consumer: c.consumer,
			Streams:  []string{c.stream, ">"},
			Count:    1,
			Block:    5 * time.Second,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) || ctx.Err() != nil {
				continue
			}
			return fmt.Errorf("queue: read group: %w", err)
		}
		for _, st := range streams {
			for _, m := range st.Messages {
				c.dispatch(ctx, m, handle)
			}
		}
	}
}

// reclaimPending sweeps one batch of the group's pending entries. Deliveries
// idle past ReclaimMinIdle are claimed to this consumer and re-dispatched;
// entries at or beyond MaxDeliveries are acknowledged away instead, so a
// message whose processing can never succeed does not loop forever. Failed
// retries reset a message's idle time, so it naturally waits another
// ReclaimMinIdle before the next attempt.
func (c *Consumer) reclaimPending(ctx context.Context, handle rawHandler) {
	pending, err := c.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: c.stream,
		Group:  c.group,
		Idle:   c.reclaimMinIdle,
		Start:  "-",
		End:    "+",
		Count:  100,
	}).Result()
	if err != nil || len(pending) == 0 {
		return
	}
	for _, p := range pending {
		if p.RetryCount >= c.maxDeliveries {
			if err := c.rdb.XAck(ctx, c.stream, c.group, p.ID).Err(); err == nil && c.Dropped != nil {
				c.Dropped(p.ID, p.RetryCount)
			}
			continue
		}
		// XCLAIM re-checks the idle time, so a message another consumer
		// claimed between XPENDING and here is skipped, not stolen.
		claimed, err := c.rdb.XClaim(ctx, &redis.XClaimArgs{
			Stream:   c.stream,
			Group:    c.group,
			Consumer: c.consumer,
			MinIdle:  c.reclaimMinIdle,
			Messages: []string{p.ID},
		}).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			continue
		}
		for _, m := range claimed {
			c.dispatch(ctx, m, handle)
		}
	}
}

func (c *Consumer) dispatch(ctx context.Context, m redis.XMessage, handle rawHandler) {
	raw, ok := m.Values["data"].(string)
	if !ok {
		// Unparseable payload: ack to avoid an infinite redelivery loop.
		_ = c.rdb.XAck(ctx, c.stream, c.group, m.ID).Err()
		return
	}
	// Continue the producer's trace: extract the context stamped into the
	// message and process under a consumer span, so the handler's spans and
	// log records correlate with the request that queued the work. Only the
	// trace envelope is decoded here — the rest of the payload belongs to
	// whichever handler understands this stream.
	var envelope struct {
		Trace map[string]string `json:"trace,omitempty"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)

	msgCtx := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(envelope.Trace))
	msgCtx, span := otel.Tracer(scopeName).Start(msgCtx, "consume "+c.stream,
		trace.WithSpanKind(trace.SpanKindConsumer),
	)
	defer span.End()

	if err := handle(msgCtx, []byte(raw)); err != nil {
		if errors.Is(err, errUnprocessable) {
			// Undecodable: ack to avoid an infinite redelivery loop.
			_ = c.rdb.XAck(ctx, c.stream, c.group, m.ID).Err()
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		// Leave the message pending for retry; do not ack.
		return
	}
	_ = c.rdb.XAck(ctx, c.stream, c.group, m.ID).Err()
}

// Close releases the underlying Redis connection.
func (c *Consumer) Close() error { return c.rdb.Close() }
