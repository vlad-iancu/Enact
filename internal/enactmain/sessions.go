package enactmain

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// sessionCookie is the browser cookie carrying the session token.
const sessionCookie = "enact_session"

// Session is one logged-in browser session.
type Session struct {
	Token       string
	UserID      string
	Email       string
	DisplayName string
	ExpiresAt   time.Time
}

// SessionStore keeps sessions in memory. Tokens are 256-bit random values;
// expiry is enforced lazily on read. In-memory is a deliberate dev trade-off
// (a restart logs everyone out) — the interface boundary is Get/Create/
// Delete, so a persistent store can replace it without touching handlers.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
	ttl      time.Duration
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &SessionStore{sessions: map[string]Session{}, ttl: ttl}
}

// Create mints a session for the given user and returns it.
func (s *SessionStore) Create(userID, email, displayName string) (Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}
	sess := Session{
		Token:       hex.EncodeToString(raw),
		UserID:      userID,
		Email:       email,
		DisplayName: displayName,
		ExpiresAt:   time.Now().Add(s.ttl),
	}
	s.mu.Lock()
	s.sessions[sess.Token] = sess
	s.mu.Unlock()
	return sess, nil
}

// Get resolves a token to its session; expired sessions are deleted and
// reported absent.
func (s *SessionStore) Get(token string) (Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return Session{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		s.Delete(token)
		return Session{}, false
	}
	return sess, true
}

// Delete removes a session; unknown tokens are a no-op.
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// DeleteByUserID revokes every session of one user (e.g. when an
// administrator deletes the account) and returns how many were removed.
func (s *SessionStore) DeleteByUserID(userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for token, sess := range s.sessions {
		if sess.UserID == userID {
			delete(s.sessions, token)
			removed++
		}
	}
	return removed
}

// cookieSettings carries the attributes shared by all auth cookies.
//
// SameSite matters when the frontend lives on a DIFFERENT REGISTRABLE
// DOMAIN than this backend: Lax cookies are not sent on cross-site fetches,
// so such deployments need None — and browsers only accept SameSite=None
// together with Secure, which in turn requires HTTPS. Same-site deployments
// (localhost ports, subdomains of one domain) should stay on Lax, which
// doubles as CSRF protection.
type cookieSettings struct {
	Secure   bool
	SameSite http.SameSite
}

// setSessionCookie writes the session cookie on a response.
func setSessionCookie(w http.ResponseWriter, sess Session, ck cookieSettings) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.Token,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		Secure:   ck.Secure,
		SameSite: ck.SameSite,
	})
}

// clearSessionCookie expires the session cookie on a response.
func clearSessionCookie(w http.ResponseWriter, ck cookieSettings) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   ck.Secure,
		SameSite: ck.SameSite,
	})
}
