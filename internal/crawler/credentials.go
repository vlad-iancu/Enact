package crawler

import (
	"net/http"
	"strings"
)

// CredentialRule is a set of headers and the URLs they may be sent to. It
// mirrors crawls.CredentialRule with the values already unsealed.
type CredentialRule struct {
	URLPattern string
	Headers    map[string]string
}

// credentialTransport attaches a crawl's headers to the requests they belong
// on, and to no others.
//
// A RoundTripper rather than a header set on the outgoing request, because
// redirects are the whole problem. http.Client copies the original request's
// headers onto each redirect hop; it strips Authorization, Cookie and
// WWW-Authenticate when the redirect crosses to a different DOMAIN, and it
// strips nothing at all for a custom header like X-Atlassian-Token, nor for a
// hop within the same registrable domain. A crawl that authenticated to an
// internal wiki and followed a redirect to a user-content subdomain would hand
// over its token.
//
// A RoundTripper sees every hop with the URL that hop is actually going to. So
// the rule is evaluated there, afresh, each time: every managed header is
// removed first and then re-applied only if THIS url matches. A credential
// cannot ride a redirect anywhere it was not addressed.
type credentialTransport struct {
	base  http.RoundTripper
	rules []CredentialRule
	// managed is every header name any rule sets, lowercased. Deleting by
	// this set rather than by the matching rule's names is deliberate: a
	// header placed by rule A must not survive into a request matching rule B.
	managed []string
}

func newCredentialTransport(base http.RoundTripper, rules []CredentialRule) *credentialTransport {
	seen := map[string]bool{}
	var managed []string
	for _, rule := range rules {
		for name := range rule.Headers {
			if key := http.CanonicalHeaderKey(name); !seen[key] {
				seen[key] = true
				managed = append(managed, key)
			}
		}
	}
	return &credentialTransport{base: base, rules: rules, managed: managed}
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Cloned because a RoundTripper must not modify the request it is given,
	// and because the client reuses the original across redirect hops.
	out := req.Clone(req.Context())
	for _, name := range t.managed {
		out.Header.Del(name)
	}
	for _, rule := range t.rules {
		if matchWildcard(rule.URLPattern, req.URL.String()) {
			for name, value := range rule.Headers {
				out.Header.Set(name, value)
			}
			break
		}
	}
	return t.base.RoundTrip(out)
}

// CredentialHosts lists the hosts a rule set sends headers to, for logging.
//
// Hosts and header NAMES are safe to log and genuinely useful — "which crawl
// is authenticating to what" is an operational question. Values never are.
func CredentialHosts(rules []CredentialRule) []string {
	var out []string
	for _, rule := range rules {
		pattern := rule.URLPattern
		if i := strings.Index(pattern, "://"); i >= 0 {
			pattern = pattern[i+3:]
		}
		if i := strings.Index(pattern, "/"); i >= 0 {
			pattern = pattern[:i]
		}
		out = append(out, pattern)
	}
	return out
}
