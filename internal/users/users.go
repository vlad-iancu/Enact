// Package users holds the user-account domain: registered users with either
// a local password (hashed) or a Google identity, persisted in OpenSearch.
// It is a domain package (not part of the main service) per the repository
// layout rules; the main service owns all reads and writes.
package users

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"enact/internal/opensearch"
)

// Config holds the OpenSearch index name for the user domain.
type Config struct {
	Index string `env:"OPENSEARCH_INDEX_USERS, default=enact-users"`
}

// User is one account. Exactly the data the platform cares about: display
// name, email, and whether the email is verified (a login precondition).
// PasswordHash is set for local accounts, GoogleSub for Google accounts; an
// account that used both login methods has both.
type User struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	PasswordHash  string `json:"password_hash,omitempty"`
	EmailVerified bool   `json:"email_verified"`
	GoogleSub     string `json:"google_sub,omitempty"`
	// AvatarKey is the storage key of the user's avatar (empty when none).
	// The public URL is derived from it at read time, so CDN reconfiguration
	// never invalidates stored records.
	AvatarKey string `json:"avatar_key,omitempty"`
	// VerificationTokenHash is the SHA-256 (hex) of the pending email
	// verification token — never the token itself, so a storage leak does
	// not yield working verification links. Empty once verified.
	VerificationTokenHash string `json:"verification_token_hash,omitempty"`
	// VerificationExpiresAt bounds the pending token's validity.
	VerificationExpiresAt time.Time `json:"verification_expires_at"`
	// APIKeys are the account's programmatic credentials. Each one
	// authenticates as this user, so the set is part of the account rather
	// than a domain of its own.
	APIKeys   []APIKey  `json:"api_keys,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// APIKey is one programmatic credential belonging to a user.
//
// The key itself is NOT stored — only its SHA-256, exactly as with
// PasswordHash and VerificationTokenHash above. A key is only ever verified,
// never replayed to anyone, so there is nothing to gain from being able to
// read it back and a great deal to lose: a storage leak would otherwise hand
// over working credentials for every account.
//
// That choice is also what makes the lookup possible. Authentication has to
// find the user FROM the key, and a hash is a value that can be matched;
// an encrypted key could not be, since sealing the same input twice produces
// different bytes.
type APIKey struct {
	// ID names the key for revocation. The hash cannot serve this purpose,
	// because listing keys must not expose anything that could authenticate.
	ID   string `json:"id"`
	Name string `json:"name"`
	// KeyHash is the SHA-256 (hex) of the presented key.
	KeyHash string `json:"key_hash"`
	// Prefix is the key's leading characters, kept so a person can tell which
	// of their keys a row refers to. Far too short to be guessable back into
	// the key.
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt is written opportunistically, not on every request — see the
	// throttle in enact-main. It answers "is this key still in use?" before a
	// revocation, which does not need to be to-the-second.
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// Repository persists users in OpenSearch. The document id is the
// normalized email, which makes email uniqueness structural (an index call
// for an existing email overwrites rather than duplicates) and lookups
// realtime (GET by id, no search-refresh lag).
type Repository struct {
	os    *opensearch.Client
	index string
}

// NewRepository returns a user Repository using the index in cfg.
func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{os: os, index: cfg.Index}
}

// EnsureIndex verifies the users index exists. The index and its mapping are
// owned by the composable index template in mappings/ and created by
// `make infrastructure-up`; this fails fast when it is missing.
func (r *Repository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("users: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// NormalizeEmail canonicalizes an email for use as identity key.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Delete removes a user record by email. Deleting an absent user is an
// error surfaced by the underlying client, so callers check existence first.
func (r *Repository) Delete(ctx context.Context, email string) error {
	return r.os.DeleteDoc(ctx, r.index, NormalizeEmail(email))
}

// List returns one page of all user records, newest first, plus the total
// account count for pagination.
func (r *Repository) List(ctx context.Context, from, size int) ([]User, int, error) {
	query := fmt.Sprintf(
		`{"from":%d,"size":%d,"sort":[{"created_at":"desc"}],"track_total_hits":true,"query":{"match_all":{}}}`,
		from, size)
	result, err := r.os.SearchWithAggregations(ctx, r.index, []byte(query))
	if err != nil {
		return nil, 0, err
	}
	users := make([]User, 0, len(result.Hits))
	for _, hit := range result.Hits {
		var u User
		if err := json.Unmarshal(hit.Source, &u); err != nil {
			return nil, 0, fmt.Errorf("users: decode %s: %w", hit.ID, err)
		}
		users = append(users, u)
	}
	return users, result.Total, nil
}

// GetByEmail fetches a user by email. The boolean reports existence.
func (r *Repository) GetByEmail(ctx context.Context, email string) (User, bool, error) {
	var u User
	found, err := r.os.GetSource(ctx, r.index, NormalizeEmail(email), &u)
	return u, found, err
}

// ByIDs fetches users by their ids, returned as a map for lookup. Documents
// are keyed by email, so this is a search rather than a get — one round trip
// for the whole set, not one per member.
//
// Ids that match no account are simply absent from the result: a membership
// can outlive the account it names, and a listing must still render.
func (r *Repository) ByIDs(ctx context.Context, ids []string) (map[string]User, error) {
	out := map[string]User{}
	if len(ids) == 0 {
		return out, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":  len(ids),
		"query": map[string]any{"terms": map[string]any{"id": ids}},
	})
	if err != nil {
		return nil, fmt.Errorf("users: marshal by-ids query: %w", err)
	}
	result, err := r.os.SearchWithAggregations(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	for _, hit := range result.Hits {
		var u User
		if err := json.Unmarshal(hit.Source, &u); err != nil {
			return nil, fmt.Errorf("users: decode %s: %w", hit.ID, err)
		}
		out[u.ID] = u
	}
	return out, nil
}

// ByAPIKeyHash finds the account holding an API key with the given hash, and
// the key entry itself. The boolean reports whether one was found.
//
// A search rather than a get: documents are keyed by email, and
// authentication starts from the key. Hashes are 256-bit values, so at most
// one document can match — but the specific entry is still selected in Go,
// because a match tells us which user, not which of their keys.
func (r *Repository) ByAPIKeyHash(ctx context.Context, keyHash string) (User, APIKey, bool, error) {
	if keyHash == "" {
		return User{}, APIKey{}, false, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":  1,
		"query": map[string]any{"term": map[string]any{"api_keys.key_hash": keyHash}},
	})
	if err != nil {
		return User{}, APIKey{}, false, fmt.Errorf("users: marshal api key query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return User{}, APIKey{}, false, err
	}
	if len(hits) == 0 {
		return User{}, APIKey{}, false, nil
	}
	var u User
	if err := json.Unmarshal(hits[0].Source, &u); err != nil {
		return User{}, APIKey{}, false, fmt.Errorf("users: decode %s: %w", hits[0].ID, err)
	}
	for _, k := range u.APIKeys {
		// Constant-time: the hash of a presented key is attacker-supplied, and
		// this is the comparison that decides whether they are someone.
		if subtle.ConstantTimeCompare([]byte(k.KeyHash), []byte(keyHash)) == 1 {
			return u, k, true, nil
		}
	}
	// The document matched but no entry did — the index is ahead of or behind
	// the source. Refusing is the only safe answer.
	return User{}, APIKey{}, false, nil
}

// AddAPIKey appends a key to an account, atomically.
//
// A read-modify-write through Save would drop a key created concurrently —
// the same lost-update bug fixed in the RBAC role repository. The script
// touches only the array, so two simultaneous creations both survive.
func (r *Repository) AddAPIKey(ctx context.Context, email string, key APIKey) error {
	entry := map[string]any{
		"id": key.ID, "name": key.Name, "key_hash": key.KeyHash,
		"prefix": key.Prefix, "created_at": key.CreatedAt,
	}
	return r.updateAPIKeys(ctx, email, `
		if (ctx._source.api_keys == null) { ctx._source.api_keys = []; }
		ctx._source.api_keys.add(params.entry);`,
		map[string]any{"entry": entry, "now": time.Now().UTC()})
}

// RemoveAPIKey deletes a key by id. Removing an id the account does not have
// is a no-op, so a repeated revoke is not an error.
func (r *Repository) RemoveAPIKey(ctx context.Context, email, keyID string) error {
	return r.updateAPIKeys(ctx, email, `
		if (ctx._source.api_keys != null) {
			ctx._source.api_keys.removeIf(k -> k.id == params.key_id);
		}`,
		map[string]any{"key_id": keyID, "now": time.Now().UTC()})
}

// TouchAPIKey records that a key was used. Callers throttle this — it is a
// write, and an unthrottled one would turn every authenticated request into
// an index operation.
func (r *Repository) TouchAPIKey(ctx context.Context, email, keyID string, at time.Time) error {
	return r.updateAPIKeys(ctx, email, `
		if (ctx._source.api_keys != null) {
			for (k in ctx._source.api_keys) {
				if (k.id == params.key_id) { k.last_used_at = params.at; }
			}
		}`,
		map[string]any{"key_id": keyID, "at": at.UTC(), "now": time.Now().UTC()})
}

// updateAPIKeys runs a scripted update against one account's key array.
// updated_at is stamped by every caller, so an account's audit trail reflects
// key changes as much as profile edits.
func (r *Repository) updateAPIKeys(ctx context.Context, email, source string, params map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"script": map[string]any{
			"lang":   "painless",
			"source": source + "\nctx._source.updated_at = params.now;",
			"params": params,
		},
	})
	if err != nil {
		return fmt.Errorf("users: marshal api key update: %w", err)
	}
	return r.os.UpdateDoc(ctx, r.index, NormalizeEmail(email), body)
}

// Save persists a user record keyed by its normalized email, overwriting
// any previous version.
func (r *Repository) Save(ctx context.Context, u User) error {
	u.Email = NormalizeEmail(u.Email)
	body, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, u.Email, body)
}
