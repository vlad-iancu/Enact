package enactexternalidentities

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// flow is one in-progress OAuth authorization. The provider redirects the
// user's BROWSER to the callback, and that request carries no platform
// identity — so everything needed to finish the exchange and attribute the
// resulting credential is parked here, keyed by the opaque state value.
type flow struct {
	State    string
	UserID   string
	Provider string
	// Verifier is the PKCE code verifier; empty when the provider record
	// disables PKCE.
	Verifier string
	// RedirectURI is where the browser goes once the callback completes
	// (the frontend), NOT the provider redirect URI.
	RedirectURI string
	Scopes      []string
	// AccessLevel is the provider-defined level the user is consenting to,
	// when one was named; recorded on the resulting identity.
	AccessLevel string
	ExpiresAt   time.Time
}

// flowStore keeps in-progress authorizations in memory. Like enact-main's
// session store this is single-instance state: a flow started on one
// replica cannot be completed on another. Acceptable because a flow lives
// for seconds and is trivially retried; the interface is Begin/Take, so a
// shared store can replace it without touching handlers.
type flowStore struct {
	mu    sync.Mutex
	flows map[string]flow
	ttl   time.Duration
}

func newFlowStore(ttl time.Duration) *flowStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &flowStore{flows: map[string]flow{}, ttl: ttl}
}

// Begin mints an unguessable state value and records the pending flow.
func (s *flowStore) Begin(f flow) (flow, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return flow{}, err
	}
	f.State = hex.EncodeToString(raw)
	f.ExpiresAt = time.Now().Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.flows[f.State] = f
	return f, nil
}

// Take resolves a state value and consumes it: a state is single-use, so a
// replayed callback cannot mint a second identity.
func (s *flowStore) Take(state string) (flow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[state]
	if !ok {
		return flow{}, false
	}
	delete(s.flows, state)
	if time.Now().After(f.ExpiresAt) {
		return flow{}, false
	}
	return f, true
}

// Pending reports how many flows are in flight (for logging).
func (s *flowStore) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flows)
}

// evictExpiredLocked drops abandoned flows; callers hold the lock.
func (s *flowStore) evictExpiredLocked() {
	now := time.Now()
	for state, f := range s.flows {
		if now.After(f.ExpiresAt) {
			delete(s.flows, state)
		}
	}
}
