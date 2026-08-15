package enactmodelinference

import (
	"sync"
)

// authWaiters wakes tool calls that are parked on a credential the user has
// not connected yet. External-identities publishes on Redis when a
// credential lands; the subscriber calls Notify, and the parked call
// re-checks.
//
// Like the OAuth flow store in enact-external-identities and the session
// store in enact-main, this is single-instance state — but here that is not
// a limitation: the waiter and the SSE connection it belongs to live in the
// same process by construction, and Redis pub/sub fans the message out to
// every replica, so whichever one holds the waiter gets it.
type authWaiters struct {
	mu      sync.Mutex
	nextID  int64
	waiters map[string]map[int64]chan struct{}
}

func newAuthWaiters() *authWaiters {
	return &authWaiters{waiters: map[string]map[int64]chan struct{}{}}
}

// Register subscribes to every key and returns a channel that fires when
// ANY of them does, plus the function that removes the registration.
//
// Callers must register BEFORE re-checking whether the credential exists: a
// credential stored in between would otherwise be missed, and the call
// would sit until the next re-check tick.
func (w *authWaiters) Register(keys []string) (<-chan struct{}, func()) {
	// Buffered: Notify never blocks, and one pending wake-up is enough
	// because the waiter re-checks the authoritative state anyway.
	ch := make(chan struct{}, 1)

	w.mu.Lock()
	w.nextID++
	id := w.nextID
	for _, key := range keys {
		if w.waiters[key] == nil {
			w.waiters[key] = map[int64]chan struct{}{}
		}
		w.waiters[key][id] = ch
	}
	w.mu.Unlock()

	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, key := range keys {
			delete(w.waiters[key], id)
			if len(w.waiters[key]) == 0 {
				delete(w.waiters, key)
			}
		}
	}
}

// Notify wakes every waiter registered on key.
func (w *authWaiters) Notify(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	woken := 0
	for _, ch := range w.waiters[key] {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending; the waiter re-checks once
		}
		woken++
	}
	return woken
}

// Pending reports how many keys have waiters (for logging).
func (w *authWaiters) Pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.waiters)
}

// waiterKey identifies one credential the way the identity service does:
// the (user, provider) pair. The NUL separator keeps ("ab","c") distinct
// from ("a","bc").
func waiterKey(userID, provider string) string {
	return userID + "\x00" + provider
}
