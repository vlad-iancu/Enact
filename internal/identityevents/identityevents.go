// Package identityevents is the notification channel that tells waiting
// consumers a user's third-party credential became available.
//
// It exists for one flow: an agent's tool call needs a credential the user
// has not connected yet, so the inference service parks the call and tells
// the UI; when the user finishes connecting, the external identity service
// publishes here and the parked call resumes.
//
// The events carry NO credential material — only the coordinates of what
// changed, so a consumer re-asks the identity service (which is the only
// component allowed to open a sealed credential).
package identityevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Config holds the Redis pub/sub settings. Addr and Password intentionally
// reuse the platform-wide Redis env names; only the channel is specific.
type Config struct {
	Addr     string `env:"REDIS_ADDR, default=localhost:6379"`
	Password string `env:"REDIS_PASSWORD"`
	Channel  string `env:"REDIS_IDENTITY_EVENTS_CHANNEL, default=enact-identity-events"`
}

// TypeIdentityStored is published when a credential is created or replaced.
const TypeIdentityStored = "identity_stored"

// Event is one identity change. The (UserID, Provider) pair is the same key
// the identity service stores under, so a waiter can match without
// interpreting anything else.
type Event struct {
	Type        string    `json:"type"`
	UserID      string    `json:"user_id"`
	Provider    string    `json:"provider"`
	AccessLevel string    `json:"access_level,omitempty"`
	Access      []string  `json:"access,omitempty"`
	At          time.Time `json:"at"`
	// Trace carries the W3C trace context of the request that published the
	// event, so a woken consumer continues the same trace.
	Trace map[string]string `json:"trace,omitempty"`
}

// Publisher announces identity changes.
type Publisher struct {
	rdb     *redis.Client
	channel string
}

// NewPublisher returns a Publisher. It opens no connection (go-redis dials
// lazily), so a service starts fine with Redis down.
func NewPublisher(cfg Config) *Publisher {
	return &Publisher{
		rdb:     redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password}),
		channel: channelOrDefault(cfg.Channel),
	}
}

// Publish announces one event, injecting the caller's trace context.
func (p *Publisher) Publish(ctx context.Context, ev Event) error {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) > 0 {
		ev.Trace = carrier
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("identityevents: marshal event: %w", err)
	}
	return p.rdb.Publish(ctx, p.channel, data).Err()
}

// Channel is the channel this publisher writes to (for logging).
func (p *Publisher) Channel() string { return p.channel }

// Close releases the connection.
func (p *Publisher) Close() error { return p.rdb.Close() }

// Subscriber consumes identity changes.
type Subscriber struct {
	rdb     *redis.Client
	channel string
}

// NewSubscriber returns a Subscriber; it connects on Run.
func NewSubscriber(cfg Config) *Subscriber {
	return &Subscriber{
		rdb:     redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password}),
		channel: channelOrDefault(cfg.Channel),
	}
}

// Channel is the channel this subscriber listens on (for logging).
func (s *Subscriber) Channel() string { return s.channel }

// Run consumes events until ctx is cancelled, invoking handler for each.
//
// It never returns because of a connection problem: Redis may be down when
// the service starts or restart under it, and the consumer of this channel
// (a tool call waiting for authorization) also re-checks on a timer, so a
// dropped subscription must degrade rather than kill the service. onError
// may be nil; it is called for each connection failure so the service can
// log at its own level.
func (s *Subscriber) Run(ctx context.Context, handler func(context.Context, Event), onError func(error)) error {
	const (
		minBackoff = time.Second
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff
	for {
		if err := s.consume(ctx, handler); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if onError != nil {
				onError(err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		backoff = minBackoff
	}
}

// consume holds one subscription open until it fails or ctx ends.
func (s *Subscriber) consume(ctx context.Context, handler func(context.Context, Event)) error {
	sub := s.rdb.Subscribe(ctx, s.channel)
	defer func() { _ = sub.Close() }()
	// Receive forces the subscription to be established, surfacing a dead
	// Redis immediately rather than on the first published message.
	if _, err := sub.Receive(ctx); err != nil {
		return err
	}
	for msg := range sub.Channel() {
		var ev Event
		if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
			continue
		}
		// Continue the publisher's trace so the resumed work joins it.
		evCtx := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(ev.Trace))
		handler(evCtx, ev)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("identityevents: subscription closed")
}

// Close releases the connection.
func (s *Subscriber) Close() error { return s.rdb.Close() }

func channelOrDefault(channel string) string {
	if channel == "" {
		return "enact-identity-events"
	}
	return channel
}
