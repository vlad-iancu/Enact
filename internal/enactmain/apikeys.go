package enactmain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/requesthelper"
	"enact/internal/users"
)

// keyPrefix marks every issued key. It is deliberately distinctive and
// constant: a key that leaks into a log, a commit or a paste can be found by
// grepping for this one string, and secret scanners key off exactly this kind
// of fixed marker.
const keyPrefix = "enact_sk_"

// keyRandomChars is the length of the random tail. 40 base62 characters is
// roughly 238 bits — far past the point where guessing is the weak link.
const keyRandomChars = 40

// keyAlphabet excludes nothing: the key is copied, never transcribed by hand.
const keyAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// displayPrefixLen is how much of a key is kept for display. Enough to tell
// two keys apart in a list, nowhere near enough to shorten a search for the
// rest.
const displayPrefixLen = len(keyPrefix) + 6

// maxKeysPerUser bounds the array on the user document. Keys live inside the
// account record, so an unbounded list would grow a document every
// authenticated request reads.
const maxKeysPerUser = 25

// apiKeyCacheTTL is how long a resolved key is trusted without re-reading the
// account. A workflow can drive many requests a second, and every one would
// otherwise be an OpenSearch query.
//
// The cost of the TTL is revocation latency: a deleted key keeps working for
// at most this long on this process. Revocation drops the entry directly, so
// in practice that only applies to another replica.
const apiKeyCacheTTL = 30 * time.Second

// lastUsedThrottle is the minimum gap between two "last used" writes for one
// key. Recording usage is worth having; recording it per request would turn
// reads into writes.
const lastUsedThrottle = 5 * time.Minute

// apiKeyCache memoizes key-hash → account. It holds the resolved session
// rather than the user record so the hot path allocates nothing.
type apiKeyCache struct {
	mu      sync.RWMutex
	entries map[string]apiKeyCacheEntry
}

type apiKeyCacheEntry struct {
	session Session
	// email and keyID identify the record to touch; the session carries
	// neither, since it is shaped for handlers rather than for storage.
	email    string
	keyID    string
	lastUsed time.Time
	expires  time.Time
}

func newAPIKeyCache() *apiKeyCache {
	return &apiKeyCache{entries: map[string]apiKeyCacheEntry{}}
}

func (c *apiKeyCache) get(hash string) (apiKeyCacheEntry, bool) {
	c.mu.RLock()
	entry, ok := c.entries[hash]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return apiKeyCacheEntry{}, false
	}
	return entry, true
}

func (c *apiKeyCache) put(hash string, entry apiKeyCacheEntry) {
	entry.expires = time.Now().Add(apiKeyCacheTTL)
	c.mu.Lock()
	c.entries[hash] = entry
	c.mu.Unlock()
}

// markUsed records a usage write against the cached entry so the throttle
// survives across requests.
func (c *apiKeyCache) markUsed(hash string, at time.Time) {
	c.mu.Lock()
	if entry, ok := c.entries[hash]; ok {
		entry.lastUsed = at
		c.entries[hash] = entry
	}
	c.mu.Unlock()
}

// forgetUser drops every cached key belonging to an account. Revocation names
// a key id, but the cache is keyed by hash — which the revoking request does
// not have, since only the holder of a key knows it.
func (c *apiKeyCache) forgetUser(userID string) {
	c.mu.Lock()
	for hash, entry := range c.entries {
		if entry.session.UserID == userID {
			delete(c.entries, hash)
		}
	}
	c.mu.Unlock()
}

// generateAPIKey returns a new key and its storage form.
func generateAPIKey() (plaintext string, key users.APIKey, err error) {
	buf := make([]byte, keyRandomChars)
	max := big.NewInt(int64(len(keyAlphabet)))
	for i := range buf {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", users.APIKey{}, fmt.Errorf("generate api key: %w", err)
		}
		buf[i] = keyAlphabet[n.Int64()]
	}
	plaintext = keyPrefix + string(buf)
	return plaintext, users.APIKey{
		ID:        uuid.NewString(),
		KeyHash:   hashAPIKey(plaintext),
		Prefix:    plaintext[:displayPrefixLen],
		CreatedAt: time.Now().UTC(),
	}, nil
}

// hashAPIKey is the one-way function the stored value is derived by.
//
// A plain SHA-256, not a password KDF: unlike a password this is 238 bits of
// uniform randomness we generated ourselves, so there is no dictionary to
// mount and nothing for a slow hash to buy. It also has to be fast — it runs
// on every authenticated request.
func hashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// apiKeyHeader is the unambiguous way to present a key.
//
// It exists because Authorization is already spoken for: S2S signs outbound
// service calls with it (internal/s2s/token.go) and validates inbound ones
// with it (internal/s2s/filter.go), and that filter is registered on every
// one of this service's WebServices. A user key arriving in Authorization is
// therefore something the S2S layer will also try to read, and fail on.
const apiKeyHeader = "X-Enact-Api-Key"

// presentedAPIKey extracts a key from the request, and reports whether it
// arrived in the Authorization header.
//
// Both forms are accepted: the dedicated header because it cannot be mistaken
// for anything else, and Authorization: Bearer because it is what every HTTP
// client and workflow tool reaches for first.
//
// A bearer value is only claimed when it carries the enact_sk_ prefix. That
// distinction is what makes it safe to share the header with S2S: a service
// token is a JWT and can never begin with the prefix, so it is left alone for
// the S2S filter to verify, and this never removes a credential it does not
// own.
func presentedAPIKey(req *restful.Request) (key string, fromAuthorization bool) {
	if direct := strings.TrimSpace(req.Request.Header.Get(apiKeyHeader)); direct != "" {
		return direct, false
	}
	header := req.Request.Header.Get("Authorization")
	const scheme = "Bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	value := strings.TrimSpace(header[len(scheme):])
	if !strings.HasPrefix(value, keyPrefix) {
		return "", false
	}
	return value, true
}

// consumeAPIKey takes the presented key off the request, returning "" when
// there is none.
//
// "Consume" is the operative word: a key that arrived in Authorization is
// removed once read. The S2S filter is registered AFTER this one
// (internal/enactmain/service.go) and reads the same header — left in place,
// it would try to verify a user's API key as an Ed25519 service token, fail,
// and 401 a request this filter had already authenticated. Removing it means
// S2S sees an ordinary anonymous browser-style request, which is what this
// is.
//
// A key in the dedicated header leaves Authorization untouched, so a request
// that legitimately carries a service token alongside one still authenticates
// as that service.
func consumeAPIKey(req *restful.Request) string {
	key, fromAuthorization := presentedAPIKey(req)
	if fromAuthorization {
		req.Request.Header.Del("Authorization")
	} else if key != "" {
		// The dedicated header is credential material too, and nothing
		// downstream of here has any use for it.
		req.Request.Header.Del(apiKeyHeader)
	}
	return key
}

// requireCaller admits a request authenticated EITHER by a browser session or
// by an API key, and is otherwise indistinguishable from requireSession.
//
// Both branches must produce the same two things — the session attribute and
// the identity on the context — because every handler downstream reads those
// and neither knows nor should know which one authenticated.
//
// Setting the context identity is security-critical rather than merely
// convenient. identity.Filter runs container-wide and has already populated
// the context from the caller's own X-User-Id header; overwriting it is what
// stops a keyed request from naming somebody else.
func (a *MainAPI) requireCaller(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	if sess, ok := a.session(req); ok {
		a.admit(req, resp, chain, sess)
		return
	}
	key := consumeAPIKey(req)
	if key == "" {
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "not logged in")
		return
	}
	sess, ok := a.sessionForAPIKey(req, key)
	if !ok {
		// Deliberately the same message a bad session gets: distinguishing
		// "no such key" from "key for a deleted account" would confirm which
		// keys exist.
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "invalid API key")
		return
	}
	a.admit(req, resp, chain, sess)
}

// optionalCaller resolves a caller when the request carries one, and admits
// the request either way.
//
// This is for the auth group, which is a mix: /auth/register and /auth/login
// must be reachable by nobody in particular, while /auth/me answers for
// whoever is asking. A filter is used rather than resolving in the handler
// because the key has to come out of the Authorization header BEFORE the S2S
// filter reads it, and filters run ahead of handlers.
//
// Deciding whether an anonymous caller is acceptable stays with each handler:
// /auth/me refuses one, /auth/keys ignores this entirely and insists on a
// cookie.
func (a *MainAPI) optionalCaller(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	if sess, ok := a.session(req); ok {
		a.admit(req, resp, chain, sess)
		return
	}
	if key := consumeAPIKey(req); key != "" {
		if sess, ok := a.sessionForAPIKey(req, key); ok {
			a.admit(req, resp, chain, sess)
			return
		}
		// An unusable key is not an error here — the handler decides. Left
		// anonymous, /auth/me answers "not logged in", which is true.
	}
	chain.ProcessFilter(req, resp)
}

// admit attaches an authenticated caller to the request and continues.
func (a *MainAPI) admit(req *restful.Request, resp *restful.Response, chain *restful.FilterChain, sess Session) {
	ctx := identity.WithUserID(req.Request.Context(), sess.UserID)
	req.SetAttribute("session", sess)
	req.Request = req.Request.WithContext(ctx)
	chain.ProcessFilter(req, resp)
}

// sessionForAPIKey resolves a presented key to the account it belongs to.
func (a *MainAPI) sessionForAPIKey(req *restful.Request, plaintext string) (Session, bool) {
	hash := hashAPIKey(plaintext)
	logger := requesthelper.Logger(req, a.logger)
	ctx := req.Request.Context()

	if entry, ok := a.apiKeys.get(hash); ok {
		a.touchAPIKey(ctx, logger, hash, entry)
		return entry.session, true
	}

	user, key, found, err := a.users.ByAPIKeyHash(ctx, hash)
	if err != nil {
		// A lookup failure is not an authentication failure, but it cannot be
		// admitted either. Logged as an error so a broken index is visible as
		// itself rather than as users reporting bad keys.
		logger.Error("failed to resolve api key", "err", err)
		return Session{}, false
	}
	if !found {
		logger.Warn("unknown api key presented", "key_prefix", displayPrefix(plaintext))
		return Session{}, false
	}
	// An unverified account cannot log in; a key must not be a way around
	// that.
	if !user.EmailVerified {
		logger.Warn("api key for an unverified account", "user_id", user.ID, "key_id", key.ID)
		return Session{}, false
	}
	entry := apiKeyCacheEntry{
		session: Session{
			UserID:      user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
		},
		email:    user.Email,
		keyID:    key.ID,
		lastUsed: key.LastUsedAt,
	}
	a.apiKeys.put(hash, entry)
	logger.Info("api key authenticated", "user_id", user.ID, "key_id", key.ID, "key_name", key.Name)
	a.touchAPIKey(ctx, logger, hash, entry)
	return entry.session, true
}

// touchAPIKey records usage, at most once per lastUsedThrottle.
//
// Detached from the request context: the write outlives the request it was
// triggered by, and a client that hangs up mid-stream should not cancel it.
func (a *MainAPI) touchAPIKey(ctx context.Context, logger *logging.Logger, hash string, entry apiKeyCacheEntry) {
	now := time.Now().UTC()
	if now.Sub(entry.lastUsed) < lastUsedThrottle {
		return
	}
	a.apiKeys.markUsed(hash, now)
	go func() {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := a.users.TouchAPIKey(writeCtx, entry.email, entry.keyID, now); err != nil {
			// Never fatal: usage bookkeeping must not decide whether a request
			// is served.
			logger.Warn("failed to record api key usage", "key_id", entry.keyID, "err", err)
		}
	}()
}

// displayPrefix shortens a key for logging. Never log a key: this is the
// prefix a user also sees in their own listing.
func displayPrefix(plaintext string) string {
	if len(plaintext) < displayPrefixLen {
		return ""
	}
	return plaintext[:displayPrefixLen]
}

// ---------------------------------------------------------------------------
// Key management
// ---------------------------------------------------------------------------

type createAPIKeyRequest struct {
	Name string `json:"name"`
}

// createAPIKeyResponse is the ONLY place a key is ever returned.
type createAPIKeyResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Key is the plaintext, shown once. It cannot be recovered afterwards —
	// only its hash is stored.
	Key       string    `json:"key"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"created_at"`
}

// apiKeyResponse describes an existing key. No hash, ever: the listing is
// readable by anything holding a session, and a hash is a verifier.
type apiKeyResponse struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

type listAPIKeysResponse struct {
	Keys []apiKeyResponse `json:"keys"`
}

// requireSessionOnly resolves a cookie session, writing the refusal itself.
//
// The key-management routes call this rather than reading the session
// attribute, so no filter change can ever make them reachable with an API
// key. That is the point: a key that could mint another key would make
// revocation meaningless, since a stolen one could issue its own replacement
// before anybody noticed it was gone.
func (a *MainAPI) requireSessionOnly(req *restful.Request, resp *restful.Response) (Session, bool) {
	sess, ok := a.session(req)
	if !ok {
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "not logged in")
		return Session{}, false
	}
	return sess, true
}

func (a *MainAPI) createAPIKey(req *restful.Request, resp *restful.Response) {
	sess, ok := a.requireSessionOnly(req, resp)
	if !ok {
		return
	}
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create api key requested")

	var body createAPIKeyRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create api key body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "name is required")
		return
	}

	user, found, err := a.users.GetByEmail(req.Request.Context(), sess.Email)
	if err != nil || !found {
		logger.Error("failed to load account for api key creation", "err", err, "found", found)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the API key")
		return
	}
	if len(user.APIKeys) >= maxKeysPerUser {
		logger.Warn("api key limit reached", "keys", len(user.APIKeys), "limit", maxKeysPerUser)
		requesthelper.WriteError(req, resp, http.StatusBadRequest,
			fmt.Sprintf("this account already has %d API keys; delete one first", len(user.APIKeys)))
		return
	}

	plaintext, key, err := generateAPIKey()
	if err != nil {
		logger.Error("failed to generate an api key", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the API key")
		return
	}
	key.Name = name
	if err := a.users.AddAPIKey(req.Request.Context(), user.Email, key); err != nil {
		logger.Error("failed to store the api key", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the API key")
		return
	}
	// Name, id and prefix only — logging the key or its hash would put a
	// working credential in the log pipeline.
	logger.Info("api key created", "key_id", key.ID, "key_name", key.Name, "key_prefix", key.Prefix)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, createAPIKeyResponse{
		ID: key.ID, Name: key.Name, Key: plaintext, Prefix: key.Prefix, CreatedAt: key.CreatedAt,
	})
}

func (a *MainAPI) listAPIKeys(req *restful.Request, resp *restful.Response) {
	sess, ok := a.requireSessionOnly(req, resp)
	if !ok {
		return
	}
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list api keys requested")

	user, found, err := a.users.GetByEmail(req.Request.Context(), sess.Email)
	if err != nil || !found {
		logger.Error("failed to load account for api key listing", "err", err, "found", found)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list API keys")
		return
	}
	out := make([]apiKeyResponse, 0, len(user.APIKeys))
	for _, k := range user.APIKeys {
		out = append(out, apiKeyResponse{
			ID: k.ID, Name: k.Name, Prefix: k.Prefix,
			CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
		})
	}
	logger.Info("api keys listed", "count", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listAPIKeysResponse{Keys: out})
}

func (a *MainAPI) deleteAPIKey(req *restful.Request, resp *restful.Response) {
	sess, ok := a.requireSessionOnly(req, resp)
	if !ok {
		return
	}
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "key_id", id)
	logger.Info("delete api key requested")

	user, found, err := a.users.GetByEmail(req.Request.Context(), sess.Email)
	if err != nil || !found {
		logger.Error("failed to load account for api key deletion", "err", err, "found", found)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete the API key")
		return
	}
	// Checked against the caller's OWN keys, so an id belonging to another
	// account is a 404 rather than a deletion.
	present := false
	for _, k := range user.APIKeys {
		if k.ID == id {
			present = true
			break
		}
	}
	if !present {
		logger.Warn("api key not found for deletion")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "API key not found")
		return
	}
	if err := a.users.RemoveAPIKey(req.Request.Context(), user.Email, id); err != nil {
		logger.Error("failed to delete the api key", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete the API key")
		return
	}
	// Before replying: a revoke that returns 204 while the key still opens
	// doors is the one outcome nobody would forgive.
	a.apiKeys.forgetUser(sess.UserID)
	logger.Info("api key deleted")
	resp.WriteHeader(http.StatusNoContent)
}
