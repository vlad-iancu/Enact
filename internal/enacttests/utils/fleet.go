package utils

import (
	"fmt"
	"net/http"
	"time"

	"gopkg.in/yaml.v3"

	"enact/internal/requesthelper"
	"enact/internal/s2s"
)

// Fleet holds the private keys of every enact service and builds HTTP
// clients that call the platform on behalf of any of them. It is the test
// service's impersonation utility: production services hold exactly one key,
// but exercising ACLs requires speaking as each caller identity.
type Fleet struct {
	signers map[string]*s2s.Signer
	ttl     time.Duration
}

// fleetKeysFile is the YAML schema of the S2S_PRIVATE_KEYS document: the
// private-key counterpart of the JWKS, assembled from s2s/keys/ by
// scripts/start-services.sh.
type fleetKeysFile struct {
	Keys []struct {
		Kid        string `yaml:"kid"`
		PrivateKey string `yaml:"private_key"`
	} `yaml:"keys"`
}

// NewFleet parses the YAML document of private keys. An empty document is an
// error — with S2S disabled, don't build a fleet at all (see Env.Client).
func NewFleet(keysYAML string, ttl time.Duration) (*Fleet, error) {
	var f fleetKeysFile
	if err := yaml.Unmarshal([]byte(keysYAML), &f); err != nil {
		return nil, fmt.Errorf("utils: parse S2S_PRIVATE_KEYS yaml: %w", err)
	}
	if len(f.Keys) == 0 {
		return nil, fmt.Errorf("utils: S2S_PRIVATE_KEYS contains no keys")
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	signers := make(map[string]*s2s.Signer, len(f.Keys))
	for _, k := range f.Keys {
		signer, err := s2s.NewSigner(k.Kid, k.PrivateKey, ttl)
		if err != nil {
			return nil, fmt.Errorf("utils: fleet key %q: %w", k.Kid, err)
		}
		signers[k.Kid] = signer
	}
	return &Fleet{signers: signers, ttl: ttl}, nil
}

// Client returns an *http.Client that makes requests AS the service named
// by `as`, addressed to `audience`: every request is signed with that
// service's private key and carries trace propagation, mirroring a real
// service-to-service call.
func (f *Fleet) Client(as, audience string, timeout time.Duration) (*http.Client, error) {
	signer, ok := f.signers[as]
	if !ok {
		return nil, fmt.Errorf("utils: no private key for identity %q", as)
	}
	return &http.Client{
		Transport: requesthelper.NewTransport(s2s.NewTransport(nil, signer, audience)),
		Timeout:   timeout,
	}, nil
}
