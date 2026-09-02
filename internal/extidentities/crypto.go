package extidentities

import "enact/internal/secrets"

// The vault moved to internal/secrets when crawls needed it too: two domains
// need to seal values and neither should import the other. These aliases keep
// this package's API and its environment variables exactly as they were.
type (
	Vault = secrets.Vault
)

// CryptoConfig is the identity service's key material. The env tags stay here
// rather than in internal/secrets, so each consumer names its own key: a crawl
// header and an OAuth refresh token are different secrets with different blast
// radii, and one leaked key should not open both.
type CryptoConfig struct {
	Key     string   `env:"IDENTITIES_ENCRYPTION_KEY"`
	KeysOld []string `env:"IDENTITIES_ENCRYPTION_KEYS_OLD"`
}

// NewVault builds the identity vault.
func NewVault(cfg CryptoConfig) (*Vault, error) {
	return secrets.NewVault(secrets.Config{Key: cfg.Key, KeysOld: cfg.KeysOld})
}
