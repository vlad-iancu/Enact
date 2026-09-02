package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// CrawlRunMessage asks the orchestrator to execute one crawl run.
//
// IDs only, for the same reasons as ExecutionMessage: the run record is the
// source of truth for what to crawl and whose authority to use, so a
// redelivery re-reads it rather than acting on a stale copy, and nothing a
// message asserts can widen what the run may do.
//
// Both the scheduler's sweep and a manual trigger publish this, so scheduled
// and hand-started runs take exactly one code path.
type CrawlRunMessage struct {
	RunID string `json:"run_id"`

	// Trace carries the W3C trace context of whatever queued the run, so the
	// crawl joins the same distributed trace as the request or sweep that
	// asked for it.
	Trace map[string]string `json:"trace,omitempty"`
}

// PublishCrawlRun appends a crawl run to the stream. Which stream that is
// comes from configuration, so this shares the Producer with the other
// workloads without sharing a queue with them.
func (p *Producer) PublishCrawlRun(ctx context.Context, msg CrawlRunMessage) error {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) > 0 {
		msg.Trace = carrier
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("queue: marshal crawl run message: %w", err)
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		Values: map[string]any{"data": data},
	}).Err()
}

// CrawlRunHandler processes one queued crawl run. Returning nil acknowledges
// it; an error leaves it pending for the reclaim sweep.
type CrawlRunHandler func(ctx context.Context, msg CrawlRunMessage) error

// RunCrawls consumes crawl runs until ctx is cancelled, reusing the same
// reclaim, retry-limit and tracing behaviour as the other consumers.
func (c *Consumer) RunCrawls(ctx context.Context, handler CrawlRunHandler) error {
	return c.run(ctx, func(ctx context.Context, raw []byte) error {
		var msg CrawlRunMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return errUnprocessable
		}
		if msg.RunID == "" {
			// Nothing identifies what to run; retrying cannot help.
			return errUnprocessable
		}
		return handler(ctx, msg)
	})
}
