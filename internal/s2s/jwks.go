package s2s

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"gopkg.in/yaml.v3"
)

// jwksFile is the YAML schema of s2s/jwks.yaml: one entry per service.
type jwksFile struct {
	Keys []struct {
		Kid       string `yaml:"kid"`
		Algorithm string `yaml:"algorithm"`
		PublicKey string `yaml:"public_key"`
	} `yaml:"keys"`
}

// parseJWKS decodes the YAML key set into kid -> Ed25519 public key.
func parseJWKS(doc string) (map[string]ed25519.PublicKey, error) {
	var f jwksFile
	if err := yaml.Unmarshal([]byte(doc), &f); err != nil {
		return nil, fmt.Errorf("s2s: parse JWKS yaml: %w", err)
	}
	if len(f.Keys) == 0 {
		return nil, fmt.Errorf("s2s: JWKS contains no keys")
	}
	keys := make(map[string]ed25519.PublicKey, len(f.Keys))
	for _, k := range f.Keys {
		if k.Kid == "" {
			return nil, fmt.Errorf("s2s: JWKS entry without kid")
		}
		if k.Algorithm != "" && k.Algorithm != "EdDSA" {
			return nil, fmt.Errorf("s2s: JWKS key %q: unsupported algorithm %q (only EdDSA)", k.Kid, k.Algorithm)
		}
		block, _ := pem.Decode([]byte(k.PublicKey))
		if block == nil {
			return nil, fmt.Errorf("s2s: JWKS key %q: public key is not valid PEM", k.Kid)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("s2s: JWKS key %q: %w", k.Kid, err)
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("s2s: JWKS key %q is %T, want Ed25519", k.Kid, parsed)
		}
		if _, dup := keys[k.Kid]; dup {
			return nil, fmt.Errorf("s2s: JWKS contains duplicate kid %q", k.Kid)
		}
		keys[k.Kid] = pub
	}
	return keys, nil
}