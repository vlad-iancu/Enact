// Package s2s implements service-to-service authentication for the enact
// platform. Every service holds an Ed25519 keypair; outbound calls carry a
// short-lived JWT signed with the caller's private key, and inbound calls
// are verified against a JWKS of every service's public key and authorized
// against a per-service ACL (default deny).
//
// The JWKS and ACL are YAML documents passed IN FULL through environment
// variables (S2S_JWKS / S2S_ACL). The files under the repository's s2s/
// directory are the distribution source; scripts/start-services.sh reads
// them into the environment. In containers, where multiline env values are
// impractical, each content variable has a *_FILE counterpart naming a
// mounted path (docker-secrets idiom); the content variable wins when both
// are set.
//
// Wiring is per service (deliberately not part of the generic service
// runtime): each service embeds Config in its own Config, calls Load at
// startup, registers Runtime.Filter on its WebService, and wraps its
// outbound domain clients with Runtime.Transport.
package s2s

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"time"

	"enact/internal/logging"
)

// Anonymous is the caller identity assigned to requests that carry no
// service token (e.g. a developer's curl/Bruno request). Routes meant to be
// reachable without a token must list it explicitly in their ACL rule.
const Anonymous = "anonymous"

// Config carries the environment-driven S2S settings. It is embedded in
// every service's Config; the values (full YAML documents and a PEM key,
// not paths) are injected by the start scripts.
type Config struct {
	// Enabled toggles service-to-service authentication. When false, Load
	// requires no key material, services must not register the enforcement
	// filter (check Runtime.Enabled), and Transport signs nothing. On by
	// default so disabling is always an explicit, visible choice.
	Enabled bool `env:"S2S_ENABLED, default=true"`

	// JWKS is the YAML document listing every service's public key.
	JWKS string `env:"S2S_JWKS"`
	// JWKSFile is a path to the JWKS document, read when JWKS is empty.
	JWKSFile string `env:"S2S_JWKS_FILE"`
	// ACL is the YAML document with this service's route access rules.
	ACL string `env:"S2S_ACL"`
	// ACLFile is a path to the ACL document, read when ACL is empty.
	ACLFile string `env:"S2S_ACL_FILE"`
	// PrivateKey is this service's PEM-encoded (PKCS#8) Ed25519 private key.
	PrivateKey string `env:"S2S_PRIVATE_KEY"`
	// PrivateKeyFile is a path to the key PEM, read when PrivateKey is empty.
	PrivateKeyFile string `env:"S2S_PRIVATE_KEY_FILE"`
	// KeyID names this service's key in the JWKS. It triples as the JWT
	// "kid" header, the "iss" claim of tokens this service signs, and the
	// expected "aud" claim of tokens it receives — by convention the
	// service name (e.g. "enact-kb-api").
	KeyID string `env:"S2S_KEY_ID"`
	// TokenTTL is the lifetime of signed tokens.
	TokenTTL time.Duration `env:"S2S_TOKEN_TTL, default=60s"`
}

// Runtime is the loaded S2S state of one service: its identity and signer,
// the platform-wide public keys, and its own ACL.
type Runtime struct {
	enabled bool
	self    string
	keys    map[string]ed25519.PublicKey
	acl     acl
	signer  *Signer
	logger  *logging.Logger
}

// Enabled reports whether S2S enforcement is on. Services consult it when
// wiring: the enforcement filter is only registered when true.
func (r *Runtime) Enabled() bool { return r.enabled }

// Load parses and validates the S2S configuration. With cfg.Enabled false
// it returns a disabled Runtime that needs no key material; otherwise it
// fails closed: missing or inconsistent material is a startup error, never
// a silently-disabled authenticator.
func Load(cfg Config, logger *logging.Logger) (*Runtime, error) {
	if !cfg.Enabled {
		logger.Warn("s2s authentication is DISABLED; all routes accept unauthenticated callers")
		return &Runtime{logger: logger}, nil
	}
	var err error
	if cfg.JWKS, err = ResolveContent(cfg.JWKS, cfg.JWKSFile, "S2S_JWKS_FILE"); err != nil {
		return nil, err
	}
	if cfg.ACL, err = ResolveContent(cfg.ACL, cfg.ACLFile, "S2S_ACL_FILE"); err != nil {
		return nil, err
	}
	if cfg.PrivateKey, err = ResolveContent(cfg.PrivateKey, cfg.PrivateKeyFile, "S2S_PRIVATE_KEY_FILE"); err != nil {
		return nil, err
	}
	switch {
	case cfg.KeyID == "":
		return nil, fmt.Errorf("s2s: S2S_KEY_ID is not set")
	case cfg.JWKS == "":
		return nil, fmt.Errorf("s2s: S2S_JWKS is not set (run `make s2s-keygen` and start via scripts/start-services.sh)")
	case cfg.PrivateKey == "":
		return nil, fmt.Errorf("s2s: S2S_PRIVATE_KEY is not set")
	case cfg.ACL == "":
		return nil, fmt.Errorf("s2s: S2S_ACL is not set")
	}

	keys, err := parseJWKS(cfg.JWKS)
	if err != nil {
		return nil, err
	}
	priv, err := parsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	// The private key must correspond to this service's JWKS entry, or every
	// callee would reject our tokens; catch the misconfiguration at startup.
	pub, ok := keys[cfg.KeyID]
	if !ok {
		return nil, fmt.Errorf("s2s: key id %q not present in JWKS", cfg.KeyID)
	}
	if !pub.Equal(priv.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("s2s: private key does not match the JWKS public key for %q", cfg.KeyID)
	}
	rules, err := parseACL(cfg.ACL)
	if err != nil {
		return nil, err
	}

	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &Runtime{
		enabled: true,
		self:    cfg.KeyID,
		keys:    keys,
		acl:     rules,
		signer:  &Signer{key: priv, kid: cfg.KeyID, ttl: ttl},
		logger:  logger,
	}, nil
}

// Transport wraps base with a RoundTripper that signs every outbound request
// for the given audience (the callee's key id). Pass nil to wrap
// http.DefaultTransport. With S2S disabled it returns base unchanged —
// outbound calls then carry no token, matching the callees' disabled
// enforcement. Compose it under the tracing transport:
//
//	kb.NewClient(cfg, runtime.Transport(nil, "enact-kb-api"))
func (r *Runtime) Transport(base http.RoundTripper, audience string) http.RoundTripper {
	if !r.enabled {
		return base
	}
	return NewTransport(base, r.signer, audience)
}

// ResolveContent returns content when set, otherwise reads the file named
// by path (skipped when also empty). varName is the *_FILE environment
// variable, used to attribute read failures to their setting.
func ResolveContent(content, path, varName string) (string, error) {
	if content != "" || path == "" {
		return content, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("s2s: read %s: %w", varName, err)
	}
	return string(data), nil
}

// parsePrivateKey decodes a PEM PKCS#8 Ed25519 private key.
func parsePrivateKey(pemStr string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("s2s: private key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("s2s: parse private key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("s2s: private key is %T, want Ed25519", parsed)
	}
	return key, nil
}
