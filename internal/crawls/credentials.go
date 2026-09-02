package crawls

import (
	"fmt"
	"net/url"
	"strings"

	"enact/internal/secrets"
)

// Bounds on what a crawl may present to a site.
const (
	MaxCredentialRules   = 10
	MaxHeadersPerRule    = 10
	MaxHeaderNameLength  = 64
	MaxHeaderValueLength = 4096
)

// forbiddenHeaders are the ones a crawl may not set.
//
// Two kinds. Host and Content-Length are the transport's to compute, and
// letting a user set them turns a header field into a request-smuggling
// primitive. The rest — User-Agent, robots-relevant identity, the hop-by-hop
// controls — are the platform's promises about how it behaves: a crawl that
// could rewrite its User-Agent could disguise itself as a browser after
// robots.txt was consulted for a crawler, and the honesty of that identifier
// is the basis on which sites tolerate being crawled at all.
var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true,
	"connection": true, "keep-alive": true, "upgrade": true, "te": true,
	"trailer": true, "proxy-authorization": true, "proxy-connection": true,
	"user-agent": true, "from": true,
}

// validateCredentials checks a crawl's credential rules.
func validateCredentials(rules []CredentialRule) (string, bool) {
	if len(rules) > MaxCredentialRules {
		return fmt.Sprintf("at most %d credential rules are allowed", MaxCredentialRules), false
	}
	for i, rule := range rules {
		if strings.TrimSpace(rule.URLPattern) == "" {
			return fmt.Sprintf("credential rule %d: url_pattern is required", i+1), false
		}
		if msg, ok := validateCredentialPattern(rule.URLPattern); !ok {
			return fmt.Sprintf("credential rule %d: %s", i+1, msg), false
		}
		if len(rule.Headers) == 0 {
			return fmt.Sprintf("credential rule %d: at least one header is required", i+1), false
		}
		if len(rule.Headers) > MaxHeadersPerRule {
			return fmt.Sprintf("credential rule %d: at most %d headers are allowed",
				i+1, MaxHeadersPerRule), false
		}
		for name, value := range rule.Headers {
			if msg, ok := validateHeader(name, value); !ok {
				return fmt.Sprintf("credential rule %d: %s", i+1, msg), false
			}
		}
	}
	return "", true
}

// validateCredentialPattern refuses patterns whose HOST is not fixed.
//
// This is the rule that keeps a secret from travelling. `https://*` or
// `*/browse/*` would present a token to any site the crawl reached, and a
// crawl reaches whatever somebody linked to. Requiring a concrete host — a
// leading wildcard label like `https://*.example.com/` is fine, since that is
// still one organisation — means a mistake in a pattern cannot become a
// disclosure to a stranger.
func validateCredentialPattern(pattern string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(pattern))
	scheme := ""
	switch {
	case strings.HasPrefix(trimmed, "https://"):
		scheme, trimmed = "https", strings.TrimPrefix(trimmed, "https://")
	case strings.HasPrefix(trimmed, "http://"):
		scheme, trimmed = "http", strings.TrimPrefix(trimmed, "http://")
	default:
		return "url_pattern must start with https:// or http:// so the host it " +
			"sends credentials to is unambiguous", false
	}
	host := trimmed
	if slash := strings.Index(host, "/"); slash >= 0 {
		host = host[:slash]
	}
	if host == "" || host == "*" {
		return "url_pattern must name a host: a credential must not be sent to every site a crawl reaches", false
	}
	// A wildcard is allowed only as a whole leading label: *.example.com.
	if strings.Contains(host, "*") {
		if !strings.HasPrefix(host, "*.") || strings.Contains(host[2:], "*") {
			return "url_pattern may only wildcard a leading subdomain, as in https://*.example.com/", false
		}
		if !strings.Contains(host[2:], ".") {
			return "url_pattern wildcards too much of the host name", false
		}
	}
	if scheme == "http" {
		// Not refused: internal sites without TLS are a real thing. Worth a
		// clear message, because the secret goes over the wire in the clear.
		return "", true
	}
	return "", true
}

// validateHeader checks one header name and value.
func validateHeader(name, value string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "a header name is empty", false
	}
	if len(trimmed) > MaxHeaderNameLength {
		return fmt.Sprintf("header name is longer than %d characters", MaxHeaderNameLength), false
	}
	if forbiddenHeaders[strings.ToLower(trimmed)] {
		return fmt.Sprintf("header %q may not be set by a crawl", trimmed), false
	}
	// A header field name is a token; anything else lets a value carry a line
	// break and inject a second header.
	for _, r := range trimmed {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(":()<>@,;\\\"/[]?={}", r) {
			return fmt.Sprintf("header name %q contains an illegal character", trimmed), false
		}
	}
	if value == "" {
		return fmt.Sprintf("header %q has no value", trimmed), false
	}
	if len(value) > MaxHeaderValueLength {
		return fmt.Sprintf("header %q is longer than %d characters", trimmed, MaxHeaderValueLength), false
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Sprintf("header %q contains a line break", trimmed), false
	}
	return "", true
}

// CredentialHostname is the host a pattern targets, for logging. Never the
// header, never the value — the host is enough to debug a rule and carries no
// secret.
func CredentialHostname(pattern string) string {
	u, err := url.Parse(strings.ReplaceAll(pattern, "*", "x"))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Sealer is the subset of a vault these helpers need. An interface so the
// domain package does not depend on a particular implementation, and so tests
// can exercise the sealing logic without key material.
type Sealer interface {
	Seal(plaintext string) (string, error)
	Open(sealed string) (string, error)
}

// SealCredentials encrypts every header value in place.
//
// Called on the way IN, once, so nothing unsealed is ever handed to the
// repository. A value that is already sealed is left alone, which is what
// makes an update that does not resend the headers a no-op rather than a
// double-encryption.
// SealCrawl seals every secret a crawl carries: its credential headers and,
// when it explores an issue tracker, its API token. One function so a new kind
// of secret cannot be added without passing through here.
func SealCrawl(vault Sealer, c *Crawl) error {
	if err := SealCredentials(vault, c.Credentials); err != nil {
		return err
	}
	if c.JIRA != nil && c.JIRA.Token != "" && !isSealed(c.JIRA.Token) {
		sealed, err := vault.Seal(c.JIRA.Token)
		if err != nil {
			return fmt.Errorf("crawls: seal the JIRA token: %w", err)
		}
		c.JIRA.Token = sealed
	}
	return nil
}

func SealCredentials(vault Sealer, rules []CredentialRule) error {
	for _, rule := range rules {
		for name, value := range rule.Headers {
			if value == "" || isSealed(value) {
				continue
			}
			sealed, err := vault.Seal(value)
			if err != nil {
				return fmt.Errorf("crawls: seal header %q: %w", name, err)
			}
			rule.Headers[name] = sealed
		}
	}
	return nil
}

// OpenCredentials returns the rules with their values decrypted, leaving the
// stored copy sealed.
//
// A value that will not open is DROPPED rather than passed through as
// ciphertext: sending a base64 blob as a bearer token would authenticate
// nothing and log the ciphertext in somebody else's access log.
func OpenCredentials(vault Sealer, rules []CredentialRule) ([]CredentialRule, error) {
	out := make([]CredentialRule, 0, len(rules))
	for _, rule := range rules {
		headers := make(map[string]string, len(rule.Headers))
		for name, value := range rule.Headers {
			opened, err := vault.Open(value)
			if err != nil {
				return nil, fmt.Errorf("crawls: open header %q: %w", name, err)
			}
			headers[name] = opened
		}
		out = append(out, CredentialRule{URLPattern: rule.URLPattern, Headers: headers})
	}
	return out, nil
}

// Redacted returns a copy of a crawl safe to send to a client.
//
// A method on the crawl rather than something each handler remembers to call,
// because there are four response paths and a leak needs only one of them to
// be forgotten — including the next one somebody adds.
func (c Crawl) Redacted() Crawl {
	c.Credentials = RedactCredentials(c.Credentials)
	if c.JIRA != nil {
		// A copy, so redacting for a response cannot blank the token we are
		// about to store.
		jira := *c.JIRA
		jira.Token = ""
		c.JIRA = &jira
	}
	return c
}

// RedactAll redacts a list, for the listing path.
func RedactAll(list []Crawl) []Crawl {
	out := make([]Crawl, 0, len(list))
	for _, c := range list {
		out = append(out, c.Redacted())
	}
	return out
}

// RedactCredentials returns the rules with every value blanked, for the API.
//
// Names and hosts survive because "what is this crawl configured to send, and
// where" is a question an owner must be able to answer. Values never leave the
// service: there is no read path for them, so a compromised session token
// cannot be turned into the credentials the crawl holds.
func RedactCredentials(rules []CredentialRule) []CredentialRule {
	out := make([]CredentialRule, 0, len(rules))
	for _, rule := range rules {
		headers := make(map[string]string, len(rule.Headers))
		for name := range rule.Headers {
			headers[name] = ""
		}
		out = append(out, CredentialRule{URLPattern: rule.URLPattern, Headers: headers})
	}
	return out
}

// isSealed recognises the vault's wire format, which is versioned and
// fingerprinted: v1.<fingerprint>.<payload>.
func isSealed(value string) bool {
	parts := strings.SplitN(value, ".", 3)
	return len(parts) == 3 && parts[0] == "v1" && parts[1] != "" && parts[2] != ""
}

// CryptoConfig is the key that seals crawl credentials.
//
// Its own variable rather than the identity service's: a crawl header and an
// OAuth refresh token are different secrets held for different reasons, and a
// key that opens one should not open the other. There is no default — a
// missing key is a startup error, never a silent plaintext fallback.
type CryptoConfig struct {
	// Key is base64 of exactly 32 random bytes: openssl rand -base64 32
	Key string `env:"CRAWL_ENCRYPTION_KEY"`
	// KeysOld are decrypt-only predecessors, so a rotation can re-seal lazily.
	KeysOld []string `env:"CRAWL_ENCRYPTION_KEYS_OLD"`
}

// NewVault builds the crawl credential vault.
func NewVault(cfg CryptoConfig) (*secrets.Vault, error) {
	return secrets.NewVault(secrets.Config{Key: cfg.Key, KeysOld: cfg.KeysOld})
}
