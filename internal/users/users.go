// Package users holds the user-account domain: registered users with either
// a local password (hashed) or a Google identity, persisted in OpenSearch.
// It is a domain package (not part of the main service) per the repository
// layout rules; the main service owns all reads and writes.
package users

import (
	"context"
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
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	PasswordHash  string    `json:"password_hash,omitempty"`
	EmailVerified bool      `json:"email_verified"`
	GoogleSub     string    `json:"google_sub,omitempty"`
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
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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

// GetByEmail fetches a user by email. The boolean reports existence.
func (r *Repository) GetByEmail(ctx context.Context, email string) (User, bool, error) {
	var u User
	found, err := r.os.GetSource(ctx, r.index, NormalizeEmail(email), &u)
	return u, found, err
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
