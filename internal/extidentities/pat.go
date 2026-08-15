package extidentities

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// patEnvelope is the JSON persisted (sealed) as Identity.Credentials for
// personal access tokens. Username carries the second half of the pair for
// providers that expect basic auth (JIRA Cloud: email + API token).
type patEnvelope struct {
	Version    int       `json:"v"`
	Token      string    `json:"token"`
	Username   string    `json:"username,omitempty"`
	ObtainedAt time.Time `json:"obtained_at"`
}

// patProvider implements Provider for tokens the user supplies directly.
type patProvider struct {
	record ProviderRecord
}

func newPATProvider(rec ProviderRecord) Provider { return &patProvider{record: rec} }

func (p *patProvider) Name() string       { return p.record.Name }
func (p *patProvider) Type() ProviderType { return ProviderTypePAT }

// StoreIdentity accepts a bare token string or {"token": …, "username": …}.
func (p *patProvider) StoreIdentity(_ context.Context, payload any, _ string) (StoredCredential, error) {
	env := patEnvelope{Version: 1, ObtainedAt: time.Now().UTC()}
	if s, ok := payload.(string); ok {
		env.Token = strings.TrimSpace(s)
	} else {
		fields, err := toStringKeyedMap(payload)
		if err != nil {
			return StoredCredential{}, err
		}
		for _, key := range []string{"token", "credentials", "access_token"} {
			if v, ok := fields[key].(string); ok && v != "" {
				env.Token = strings.TrimSpace(v)
				break
			}
		}
		if v, ok := fields["username"].(string); ok {
			env.Username = strings.TrimSpace(v)
		}
	}
	if env.Token == "" {
		return StoredCredential{}, fmt.Errorf("extidentities: pat payload has no token")
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return StoredCredential{}, fmt.Errorf("extidentities: marshal pat envelope: %w", err)
	}
	// A PAT has no expiry the platform can see and nothing to refresh: the
	// provider revokes it out of band, and the first rejected API call is
	// how a consumer finds out.
	return StoredCredential{Envelope: string(raw)}, nil
}

func (p *patProvider) RetrieveIdentity(_ context.Context, envelope string) (Credential, error) {
	var env patEnvelope
	if err := json.Unmarshal([]byte(envelope), &env); err != nil {
		return Credential{}, fmt.Errorf("extidentities: stored pat envelope is unreadable: %w", err)
	}
	scheme := p.record.PAT.scheme()
	return Credential{
		Credentials: env.Token,
		TokenType:   scheme,
		Username:    env.Username,
	}, nil
}

// Refresh is the documented no-op: a PAT lives until the user revokes it.
func (p *patProvider) Refresh(context.Context, string) (StoredCredential, bool, error) {
	return StoredCredential{}, false, nil
}

// Revoke cannot be done here: a personal access token is issued and deleted
// in the provider's own UI, and there is no standard API for a third party
// holding a copy to invalidate it. Deleting the identity is the whole of
// what this platform can do; the token stays live until the user removes it
// at the provider.
func (p *patProvider) Revoke(context.Context, string) error {
	return ErrRevocationUnsupported
}

// scheme reports how the token is presented, defaulting to bearer.
func (c *PATConfig) scheme() string {
	if c == nil || c.Scheme == "" {
		return "bearer"
	}
	return strings.ToLower(c.Scheme)
}
