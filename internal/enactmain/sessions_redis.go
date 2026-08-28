package enactmain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"enact/internal/logging"
)

// SessionsConfig points the session store at Redis.
//
// Deliberately no default: an empty address means the in-process store, so a
// bare checkout with no Redis still runs. Setting it is what opts in.
type SessionsConfig struct {
	RedisAddr     string `env:"SESSION_REDIS_ADDR"`
	RedisPassword string `env:"SESSION_REDIS_PASSWORD"`
	// RedisDB isolates sessions from the work queues when they share a server.
	RedisDB int `env:"SESSION_REDIS_DB, default=0"`
}

// Key prefixes. Namespaced because this Redis is shared with the work queues.
const (
	sessionKeyPrefix = "enact:session:"
	// userSessionsPrefix indexes a user's live tokens. Redis can find a value
	// by key and no other way, so revoking every session of one account —
	// which an administrator deleting it must do — needs this second record.
	userSessionsPrefix = "enact:session-user:"
)

// redisSessionStore keeps sessions in Redis, so they survive a restart and are
// shared by every replica.
//
// Expiry is Redis's job rather than lazy bookkeeping: each key carries a TTL,
// so a session disappears on time even if nobody ever asks for it again.
type redisSessionStore struct {
	rdb    *redis.Client
	ttl    time.Duration
	logger *logging.Logger
}

// NewRedisSessionStore connects to Redis and verifies it answers.
//
// The ping matters: without it a misconfigured address would surface as every
// login silently failing to stick, which looks exactly like the in-memory
// behaviour this exists to fix. Better to refuse to start.
func NewRedisSessionStore(ctx context.Context, cfg SessionsConfig, ttl time.Duration, logger *logging.Logger) (SessionStore, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("enactmain: session store at %s: %w", cfg.RedisAddr, err)
	}
	return &redisSessionStore{rdb: rdb, ttl: ttl, logger: logger}, nil
}

func (s *redisSessionStore) Create(userID, email, displayName string) (Session, error) {
	sess, err := newSession(userID, email, displayName, s.ttl)
	if err != nil {
		return Session{}, err
	}
	body, err := json.Marshal(sess)
	if err != nil {
		return Session{}, fmt.Errorf("enactmain: marshal session: %w", err)
	}
	ctx, cancel := s.context()
	defer cancel()

	// The session and the user's index are written together: an index entry
	// without its session is a token that resolves to nothing, and a session
	// without its index entry is one that a delete-account would miss.
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, sessionKeyPrefix+sess.Token, body, s.ttl)
	pipe.SAdd(ctx, userSessionsPrefix+userID, sess.Token)
	// The index outlives the longest session it holds, and is refreshed on
	// every login, so it cannot accumulate for an account that stops logging
	// in. Individual tokens inside it may already have expired; Get treats a
	// missing session as absent, and DeleteByUserID counts only real deletes.
	pipe.Expire(ctx, userSessionsPrefix+userID, s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return Session{}, fmt.Errorf("enactmain: store session: %w", err)
	}
	return sess, nil
}

func (s *redisSessionStore) Get(token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	ctx, cancel := s.context()
	defer cancel()

	body, err := s.rdb.Get(ctx, sessionKeyPrefix+token).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			// An outage logs everybody out, which is bad enough without it
			// being invisible: this is the difference between "Redis is down"
			// and "my cookie stopped working".
			s.logger.Error("failed to read a session from redis", "err", err)
		}
		return Session{}, false
	}
	var sess Session
	if err := json.Unmarshal(body, &sess); err != nil {
		s.logger.Error("stored session is unreadable; discarding it", "err", err)
		s.Delete(token)
		return Session{}, false
	}
	// Redis expiry is authoritative, but the stored ExpiresAt is checked too:
	// a key restored from a backup could outlive the session it represents.
	if time.Now().After(sess.ExpiresAt) {
		s.Delete(token)
		return Session{}, false
	}
	return sess, true
}

func (s *redisSessionStore) Delete(token string) {
	if token == "" {
		return
	}
	ctx, cancel := s.context()
	defer cancel()

	// Read first, only to learn whose token this is — otherwise the user's
	// index keeps a token that no longer exists, and a later delete-account
	// would report revoking more sessions than it did.
	var sess Session
	if body, err := s.rdb.Get(ctx, sessionKeyPrefix+token).Bytes(); err == nil {
		_ = json.Unmarshal(body, &sess)
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, sessionKeyPrefix+token)
	if sess.UserID != "" {
		pipe.SRem(ctx, userSessionsPrefix+sess.UserID, token)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		s.logger.Error("failed to delete a session", "err", err)
	}
}

func (s *redisSessionStore) DeleteByUserID(userID string) int {
	if userID == "" {
		return 0
	}
	ctx, cancel := s.context()
	defer cancel()

	tokens, err := s.rdb.SMembers(ctx, userSessionsPrefix+userID).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		s.logger.Error("failed to list a user's sessions", "user_id", userID, "err", err)
		return 0
	}
	if len(tokens) == 0 {
		return 0
	}
	keys := make([]string, 0, len(tokens))
	for _, token := range tokens {
		keys = append(keys, sessionKeyPrefix+token)
	}
	// The DEL reply counts keys that actually existed, so an index entry whose
	// session already expired is not reported as a revocation.
	removed, err := s.rdb.Del(ctx, keys...).Result()
	if err != nil {
		s.logger.Error("failed to revoke a user's sessions", "user_id", userID, "err", err)
		return 0
	}
	if err := s.rdb.Del(ctx, userSessionsPrefix+userID).Err(); err != nil {
		s.logger.Warn("failed to clear a user's session index", "user_id", userID, "err", err)
	}
	return int(removed)
}

// context bounds every call. The store sits on the authentication path of
// every request, so a wedged Redis must fail fast rather than hold requests
// open until something else times out.
func (s *redisSessionStore) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}
