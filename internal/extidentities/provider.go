package extidentities

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// StoredCredential is what StoreIdentity and Refresh produce: the string to
// persist, plus the metadata lifted out of it so the refresh sweep and
// consumers can query without decrypting anything.
//
// Envelope is PLAINTEXT. Repository.SaveIdentity seals it before it reaches
// OpenSearch, so providers never touch the encryption key and there is no
// path where one forgets to encrypt.
type StoredCredential struct {
	Envelope  string
	ExpiresAt *time.Time
	Access    []string
	// AccessFromProvider reports that Access is what the PROVIDER said it
	// granted, rather than what the platform asked for or assumed. It is
	// what stops a declined permission from being recorded as granted:
	// callers must not overwrite provider-reported access with their own
	// wish list.
	AccessFromProvider bool
	Refreshable        bool
}

// Credential is what RetrieveIdentity returns: exactly what a consumer must
// present, and nothing more. For OAuth that is the access token — never the
// refresh token, which stays inside this domain.
type Credential struct {
	Credentials string   `json:"credentials"`
	TokenType   string   `json:"token_type,omitempty"`
	Username    string   `json:"username,omitempty"`
	Access      []string `json:"access,omitempty"`
	// AccessLevel is the provider-defined level this credential was
	// obtained for, when one was used. Set by the service, not the provider.
	AccessLevel string     `json:"access_level,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// Provider is the per-type credential handler: one instance is built from
// one stored ProviderRecord.
type Provider interface {
	// Name is the provider record's name ("google", "github").
	Name() string
	// Type reports the credential mechanics ("pat", "oauth").
	Type() ProviderType

	// StoreIdentity normalizes a caller-supplied payload into the string
	// persisted as Identity.Credentials. For OAuth the payload is the
	// callback tuple — a map with access_token, refresh_token, expiry (or
	// expires_in), scope and any provider extras. For PAT it is the token,
	// either bare or {"token": …, "username": …}.
	//
	// previous is the identity's current envelope when one exists (empty
	// otherwise); providers use it to carry forward values the new payload
	// omits, notably a refresh token that a provider only ever issues once.
	StoreIdentity(ctx context.Context, payload any, previous string) (StoredCredential, error)

	// RetrieveIdentity parses an envelope produced by StoreIdentity and
	// returns the usable credential.
	RetrieveIdentity(ctx context.Context, envelope string) (Credential, error)

	// Refresh renews a credential. refreshed=false means "nothing to do" —
	// the no-op for credentials that never expire — and the caller then
	// leaves the stored record untouched.
	Refresh(ctx context.Context, envelope string) (out StoredCredential, refreshed bool, err error)

	// Revoke asks the PROVIDER to invalidate the credentials in envelope, so
	// forgetting a credential locally also ends the access it grants.
	//
	// ErrRevocationUnsupported means the mechanism does not exist for this
	// provider type (a pasted PAT), not that anything went wrong. Callers
	// must treat every other error as advisory too: revocation is a call to
	// a third party, and a provider outage must never stop the platform from
	// deleting its own copy of a credential.
	Revoke(ctx context.Context, envelope string) error
}

// ErrRevocationUnsupported reports that a provider offers no way to revoke a
// credential: a personal access token the user pasted, or an OAuth record
// registered without a revocation endpoint. Distinct from a failure, so a
// caller can log "nothing to revoke" rather than "revocation failed".
var ErrRevocationUnsupported = errors.New("extidentities: this provider cannot revoke credentials")

// PermanentRefreshError marks a refusal that must not be retried: the
// refresh token was revoked or the user removed the application. The sweep
// marks such identities StatusRevoked instead of retrying forever.
type PermanentRefreshError struct{ Reason string }

func (e *PermanentRefreshError) Error() string { return "refresh permanently failed: " + e.Reason }

// NewProvider builds the handler for a stored provider record. vault opens
// the sealed OAuth client secret; hc is the outbound HTTP client (third
// parties are not platform services, so it carries no S2S signing).
func NewProvider(rec ProviderRecord, vault *Vault, hc *http.Client) (Provider, error) {
	switch rec.Type {
	case ProviderTypePAT:
		return newPATProvider(rec), nil
	case ProviderTypeOAuth:
		return newOAuthProvider(rec, vault, hc)
	default:
		return nil, fmt.Errorf("extidentities: unsupported provider type %q", rec.Type)
	}
}
