// Package extidentities holds the external-identity domain: the credentials
// a user holds at third-party providers (Google Workspace, GitHub, JIRA, …)
// and the provider records describing how those credentials are obtained
// and renewed.
//
// Not to be confused with internal/identity, which carries the CALLING
// user's id on the request context. This package is about credentials the
// platform holds ON BEHALF of a user at someone else's service.
//
// Credentials never leave this domain in plaintext form: Identity.Credentials
// is always the sealed envelope produced by Vault, and everything the
// platform needs to query on (expiry, type, status) is lifted out of the
// envelope into plain indexed fields.
package extidentities

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Config holds the OpenSearch index names for the external-identity domain.
type Config struct {
	IdentitiesIndex string `env:"OPENSEARCH_INDEX_IDENTITIES, default=enact-identities"`
	ProvidersIndex  string `env:"OPENSEARCH_INDEX_IDENTITY_PROVIDERS, default=enact-identity-providers"`
}

// ProviderType selects the credential mechanics of a provider.
type ProviderType string

const (
	// ProviderTypePAT is a long-lived token the user pastes in; nothing to
	// refresh.
	ProviderTypePAT ProviderType = "pat"
	// ProviderTypeOAuth is an authorization-code identity with an access
	// token, usually a refresh token, and an expiry.
	ProviderTypeOAuth ProviderType = "oauth"
)

// ValidProviderType reports whether t names a supported provider type.
func ValidProviderType(t ProviderType) bool {
	return t == ProviderTypePAT || t == ProviderTypeOAuth
}

// Identity status values.
const (
	// StatusActive means the stored credential is usable.
	StatusActive = "active"
	// StatusRefreshFailed marks transient refresh failures; the previous
	// credential is still stored and served.
	StatusRefreshFailed = "refresh_failed"
	// StatusRevoked means the provider permanently rejected the refresh
	// token (invalid_grant): only re-consent recovers.
	StatusRevoked = "revoked"
)

// Identity is one user's credential at one provider. (UserID, Provider) is
// unique and ID is derived from it, so storing the same pair twice
// overwrites — a user authenticates to a provider once, and everything that
// needs that provider (every MCP server, say) shares the one credential.
type Identity struct {
	ID           string       `json:"id"`
	UserID       string       `json:"user_id"`
	Provider     string       `json:"provider"`
	ProviderType ProviderType `json:"provider_type"`
	// Credentials is the SEALED envelope. Plaintext never reaches this
	// field: Repository.SaveIdentity seals and GetIdentity opens.
	Credentials string `json:"credentials"`
	// Access lists what the credential is actually good for: for OAuth, the
	// scopes the PROVIDER reported granting — not the ones requested, which
	// a user may have declined at the consent screen.
	Access []string `json:"access,omitempty"`
	// AccessLevel names the provider-defined level this credential was
	// obtained for, when one was used. Empty means raw scopes were used.
	AccessLevel string `json:"access_level,omitempty"`

	// ExpiresAt is the access credential's expiry, kept outside the
	// envelope and indexed so the refresh sweep can range-query it. Nil
	// means it does not expire (PATs).
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Refreshable reports whether a refresh is possible at all (an OAuth
	// identity with a refresh token).
	Refreshable bool `json:"refreshable"`

	Status          string     `json:"status"`
	RefreshFailures int        `json:"refresh_failures,omitempty"`
	RefreshFailedAt *time.Time `json:"refresh_failed_at,omitempty"`
	LastRefreshedAt *time.Time `json:"last_refreshed_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProviderRecord describes a registered identity provider; Name is the
// document id ("google", "github", "jira").
type ProviderRecord struct {
	Name        string       `json:"name"`
	Type        ProviderType `json:"type"`
	DisplayName string       `json:"display_name,omitempty"`
	OAuth       *OAuthConfig `json:"oauth,omitempty"`
	PAT         *PATConfig   `json:"pat,omitempty"`
	// AccessLevels are the named tiers the provider's registrar defined,
	// each mapping to the concrete scopes it needs ("read" -> [repo:status,
	// read:org]). They give callers and the UI a vocabulary that is not
	// provider-specific; what is stored and checked is always the concrete
	// scopes, because that is the only thing the provider understands.
	//
	// Useful for both types: for OAuth a level decides what consent asks
	// for, for PAT it labels what the pasted token is believed to cover.
	AccessLevels map[string][]string `json:"access_levels,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	UpdatedAt    time.Time           `json:"updated_at"`
}

// OAuthConfig is what a provider's well-known discovery document yields,
// plus this platform's client registration with that provider.
type OAuthConfig struct {
	Issuer       string `json:"issuer,omitempty"`
	DiscoveryURL string `json:"discovery_url,omitempty"`
	AuthorizeURL string `json:"authorize_url"`
	TokenURL     string `json:"token_url"`
	RevokeURL    string `json:"revoke_url,omitempty"`
	UserInfoURL  string `json:"userinfo_url,omitempty"`
	ClientID     string `json:"client_id"`
	// ClientSecretEnc is sealed with the same key as the identities and is
	// never indexed, never returned by the API, never logged.
	ClientSecretEnc string   `json:"client_secret_enc,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	UsePKCE         bool     `json:"use_pkce"`
	// AccessType "offline" is what makes most providers issue a refresh
	// token; Prompt "consent" is what makes them re-issue one.
	AccessType string `json:"access_type,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	// AuthStyle is "auto" (default), "header" or "params" — how client
	// credentials are presented at the token endpoint.
	AuthStyle       string            `json:"auth_style,omitempty"`
	ExtraAuthParams map[string]string `json:"extra_auth_params,omitempty"`
}

// PATConfig describes how a personal access token is presented to the
// provider. It is metadata for consumers — this service only stores the
// token.
type PATConfig struct {
	// Scheme is "bearer" (GitHub), "basic" (JIRA Cloud: email + token), or
	// "token"; HeaderName defaults to Authorization.
	Scheme     string `json:"scheme,omitempty"`
	HeaderName string `json:"header_name,omitempty"`
	DocsURL    string `json:"docs_url,omitempty"`
}

// DocID derives an identity's document id from its uniqueness pair. The NUL
// separator keeps ("ab","c") distinct from ("a","bc").
func DocID(userID, provider string) string {
	sum := sha256.Sum256([]byte(userID + "\x00" + provider))
	return hex.EncodeToString(sum[:])
}

// providerNamePattern constrains provider names: short, URL-safe, stable.
var providerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateProviderName checks a provider name against the allowed shape.
func ValidateProviderName(name string) error {
	if !providerNamePattern.MatchString(name) {
		return fmt.Errorf("name must be 1-64 characters: lowercase letters, digits, '-' or '_', starting alphanumeric")
	}
	return nil
}

// accessLevelPattern constrains level names: short, lowercase, quotable in
// a URL without escaping.
var accessLevelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidateAccessLevelName checks one access level name against the allowed
// shape. Exported so callers that reference a level (an MCP tool's access
// requirement, say) validate it against the same rule as its definition.
func ValidateAccessLevelName(name string) error {
	if !accessLevelPattern.MatchString(name) {
		return fmt.Errorf("must be 1-32 characters: lowercase letters, digits, '-' or '_', starting alphanumeric")
	}
	return nil
}

// ValidateAccessLevels checks a provider's level definitions: usable names,
// and every level actually granting something.
func ValidateAccessLevels(levels map[string][]string) error {
	for name, scopes := range levels {
		if !accessLevelPattern.MatchString(name) {
			return fmt.Errorf("access level %q must be 1-32 characters: lowercase letters, digits, '-' or '_', starting alphanumeric", name)
		}
		if len(scopes) == 0 {
			return fmt.Errorf("access level %q defines no scopes", name)
		}
		for _, scope := range scopes {
			if strings.TrimSpace(scope) == "" {
				return fmt.Errorf("access level %q contains an empty scope", name)
			}
		}
	}
	return nil
}

// ResolveAccess expands a caller's access terms into concrete scopes: a
// term matching one of the provider's level names becomes that level's
// scopes, anything else passes through as a raw scope. Callers may
// therefore speak levels, scopes, or a mix.
//
// The second return value is the single level name when the caller named
// exactly one and nothing else — what gets recorded on the identity.
func ResolveAccess(rec ProviderRecord, terms []string) ([]string, string) {
	var scopes []string
	seen := map[string]bool{}
	level := ""
	levelCount, rawCount := 0, 0

	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		if defined, ok := rec.AccessLevels[term]; ok {
			levelCount++
			level = term
			for _, scope := range defined {
				if !seen[scope] {
					seen[scope] = true
					scopes = append(scopes, scope)
				}
			}
			continue
		}
		rawCount++
		if !seen[term] {
			seen[term] = true
			scopes = append(scopes, term)
		}
	}
	if levelCount != 1 || rawCount > 0 {
		level = ""
	}
	return scopes, level
}

// HasAccess reports whether granted covers every entry of required.
func HasAccess(granted, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]bool, len(granted))
	for _, g := range granted {
		have[g] = true
	}
	for _, r := range required {
		if !have[r] {
			return false
		}
	}
	return true
}
