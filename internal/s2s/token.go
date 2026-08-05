package s2s

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Signer mints the short-lived tokens this service presents to others.
type Signer struct {
	key ed25519.PrivateKey
	kid string
	ttl time.Duration
}

// NewSigner builds a Signer for the given identity from a PEM-encoded
// (PKCS#8) Ed25519 private key. Production services get their signer via
// Load; this constructor exists for tooling that holds key material of
// other services (the enact-tests impersonation utility).
func NewSigner(kid, privateKeyPEM string, ttl time.Duration) (*Signer, error) {
	if kid == "" {
		return nil, fmt.Errorf("s2s: signer needs a key id")
	}
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &Signer{key: key, kid: kid, ttl: ttl}, nil
}

// Token signs a fresh JWT for one call to the given audience:
// iss = this service, aud = callee, exp = now+TTL, jti = random,
// header kid = this service's JWKS key id.
func (s *Signer) Token(audience string) (string, error) {
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.RegisteredClaims{
		Issuer:    s.kid,
		Audience:  jwt.ClaimStrings{audience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
		ID:        uuid.NewString(),
	})
	t.Header["kid"] = s.kid
	signed, err := t.SignedString(s.key)
	if err != nil {
		return "", fmt.Errorf("s2s: sign token: %w", err)
	}
	return signed, nil
}

// NewTransport returns a RoundTripper that stamps every outbound request
// with a freshly signed bearer token for the given audience. Tokens are
// minted per request, so each carries a unique jti and the tightest
// possible expiry. Pass nil to wrap http.DefaultTransport.
func NewTransport(base http.RoundTripper, signer *Signer, audience string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, signer: signer, audience: audience}
}

type transport struct {
	base     http.RoundTripper
	signer   *Signer
	audience string
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	token, err := t.signer.Token(t.audience)
	if err != nil {
		return nil, err
	}
	// Clone so header mutation never affects the caller's request.
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(r)
}