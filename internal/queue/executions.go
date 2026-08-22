package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// ExecutionMessage asks a runner to execute one workflow run.
//
// It carries IDS ONLY, deliberately. The execution record in OpenSearch is
// the source of truth for what to run and who to run it as; putting the steps
// or the trigger input on the message would mean a redelivery could act on a
// copy that no longer matches the record. It also keeps the authority in one
// place: the runner reads UserID from the stored record it fetches, not from
// anything a message could assert.
type ExecutionMessage struct {
	ExecutionID string `json:"execution_id"`

	// Trace carries the W3C trace context of the request that queued the run,
	// so the runner's work joins the same distributed trace as the trigger.
	Trace map[string]string `json:"trace,omitempty"`
}

// PublishExecution appends an execution message to the stream.
//
// Which stream that is comes from configuration: the workflow services point
// REDIS_STREAM/REDIS_GROUP at their own stream, so this shares the Producer
// with document indexing without sharing a queue with it.
func (p *Producer) PublishExecution(ctx context.Context, msg ExecutionMessage) error {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) > 0 {
		msg.Trace = carrier
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("queue: marshal execution message: %w", err)
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		Values: map[string]any{"data": data},
	}).Err()
}

// ExecutionHandler processes one queued execution. Returning nil acknowledges
// it; returning an error leaves it pending for the reclaim sweep to retry.
type ExecutionHandler func(ctx context.Context, msg ExecutionMessage) error

// RunExecutions consumes workflow executions until ctx is cancelled, reusing
// the same reclaim, retry-limit and tracing behaviour as document indexing.
func (c *Consumer) RunExecutions(ctx context.Context, handler ExecutionHandler) error {
	return c.run(ctx, func(ctx context.Context, raw []byte) error {
		var msg ExecutionMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return errUnprocessable
		}
		if msg.ExecutionID == "" {
			// Nothing identifies what to run; retrying cannot help.
			return errUnprocessable
		}
		return handler(ctx, msg)
	})
}
