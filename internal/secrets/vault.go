// Package secrets seals the values the platform holds on somebody else's
// behalf: third-party identity credentials, and the headers a crawl presents
// to an authenticated site.
//
// It is its own package because two domains now need it and neither should
// import the other — see ADR-0009 on where shared code lives.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Config carries the encryption key material. There is no default:
// a missing or malformed key is a startup error, never a silent plaintext
// fallback — the whole point of this service is that a leaked OpenSearch
// snapshot yields nothing usable.
type Config struct {
	// Key is base64 (standard encoding) of exactly 32 random bytes:
	//   openssl rand -base64 32
	Key string
	// KeysOld are decrypt-only predecessors, so a key rotation can re-seal
	// records lazily instead of stranding them. Comma-separated base64.
	KeysOld []string
}

// sealFormatVersion prefixes every sealed value. It is the extension point:
// a v2 may change cipher or KDF while v1 values stay readable.
const sealFormatVersion = "v1"

// Vault seals and opens the opaque values this domain stores: identity
// credential envelopes and provider client secrets.
//
// Wire format: v1.<fingerprint>.<base64url(nonce || ciphertext+tag)>
// The fingerprint is the first 8 hex characters of sha256(key) — it names
// WHICH key sealed a value (never the key itself), so "sealed with a key we
// no longer have" is diagnosable without trial decryption.
type Vault struct {
	primary keyEntry
	// openers is every key that may open a value, keyed by fingerprint:
	// the primary plus any decrypt-only predecessors.
	openers map[string]keyEntry
}

type keyEntry struct {
	fingerprint string
	aead        cipher.AEAD
}

// NewVault builds a Vault from cfg. It fails when the primary key is
// absent or not exactly 32 bytes of base64.
func NewVault(cfg Config) (*Vault, error) {
	primary, err := newKeyEntry(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("secrets: encryption key: %w", err)
	}
	v := &Vault{primary: primary, openers: map[string]keyEntry{primary.fingerprint: primary}}
	for i, old := range cfg.KeysOld {
		if strings.TrimSpace(old) == "" {
			continue
		}
		entry, err := newKeyEntry(old)
		if err != nil {
			return nil, fmt.Errorf("secrets: encryption keyS_OLD[%d]: %w", i, err)
		}
		if _, exists := v.openers[entry.fingerprint]; !exists {
			v.openers[entry.fingerprint] = entry
		}
	}
	return v, nil
}

// newKeyEntry decodes one base64 key into an AES-GCM cipher plus its
// fingerprint.
func newKeyEntry(encoded string) (keyEntry, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return keyEntry{}, fmt.Errorf("not valid base64: %w", err)
	}
	if len(raw) != 32 {
		return keyEntry{}, fmt.Errorf("must decode to exactly 32 bytes, got %d", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return keyEntry{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return keyEntry{}, err
	}
	sum := sha256.Sum256(raw)
	return keyEntry{fingerprint: hex.EncodeToString(sum[:])[:8], aead: aead}, nil
}

// KeyFingerprint reports the primary key's fingerprint. Safe to log — it
// identifies the key without revealing it.
func (v *Vault) KeyFingerprint() string { return v.primary.fingerprint }

// Seal encrypts plaintext with the primary key. An empty input seals to an
// empty string so optional fields stay optional.
func (v *Vault) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, v.primary.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("extidentities: generate nonce: %w", err)
	}
	sealed := v.primary.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return sealFormatVersion + "." + v.primary.fingerprint + "." +
		base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal, selecting the key by fingerprint.
func (v *Vault) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	parts := strings.SplitN(sealed, ".", 3)
	if len(parts) != 3 || parts[0] != sealFormatVersion {
		return "", fmt.Errorf("extidentities: sealed value is not in %s.<key>.<payload> form", sealFormatVersion)
	}
	entry, ok := v.openers[parts[1]]
	if !ok {
		// The value was sealed with a key this process does not hold —
		// naming the fingerprints makes a botched rotation obvious.
		return "", fmt.Errorf("extidentities: no key with fingerprint %q (have %s)", parts[1], strings.Join(v.fingerprints(), ","))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("extidentities: sealed payload is not valid base64: %w", err)
	}
	nonceSize := entry.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("extidentities: sealed payload is too short")
	}
	plaintext, err := entry.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("extidentities: decryption failed (wrong key or tampered value)")
	}
	return string(plaintext), nil
}

// SealedWithPrimary reports whether a sealed value used the current primary
// key — the signal a re-seal pass would act on after a rotation.
func (v *Vault) SealedWithPrimary(sealed string) bool {
	parts := strings.SplitN(sealed, ".", 3)
	return len(parts) == 3 && parts[1] == v.primary.fingerprint
}

func (v *Vault) fingerprints() []string {
	out := make([]string, 0, len(v.openers))
	for fp := range v.openers {
		out = append(out, fp)
	}
	return out
}
