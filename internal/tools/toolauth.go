package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"enact/internal/extidentities"
)

// This file is the single implementation of the tool-authorization
// contract, shared by three services deliberately: the registry validates
// with it, inference renders with it, and the proxy decodes with it. One
// copy means "it validated" genuinely implies "it will render".

// HeaderToolAuth carries the credential headers inference wants the proxy
// to place on the UPSTREAM request: base64url(JSON) of a header->value map.
//
// It exists because internal/s2s's transport is the INNERMOST RoundTripper
// on the inference->registry hop and overwrites Authorization with the
// platform's own service token — so credentials cannot travel under their
// real names. The proxy decodes this, strips the platform's headers, and
// sets the real ones on the third-party request.
const HeaderToolAuth = "X-Enact-Tool-Auth"

// maxAuthEnvelopeBytes bounds the encoded envelope; credentials are small
// and an unbounded header is a denial-of-service surface.
const maxAuthEnvelopeBytes = 8 << 10

var (
	// headerNamePattern is the RFC 9110 token subset the platform accepts.
	headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	// paramNamePattern matches a JSON argument name a tool schema would use.
	paramNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
)

// reservedHeaders may not be set by a tool authorization: they are either
// hop-by-hop, framing, or the platform's own namespace. Authorization is
// deliberately NOT reserved — it is the common case for an upstream MCP
// server, and the proxy strips the platform's own before applying this one.
var reservedHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Host", "Content-Length", "Content-Type", "Accept",
	"X-User-Id",
}

// Credentials maps provider name to the resolved credential; it is the data
// a tool's templates are rendered against.
type Credentials map[string]extidentities.Credential

// Render renders one tool's authorization into the headers and params to
// send. It is used at registration time against sentinel credentials (so
// validation proves rendering works) and at call time against the real
// ones.
func Render(auth ToolAuthorization, creds Credentials) (headers, params map[string]string, err error) {
	headers = make(map[string]string, len(auth.HeadersAuthorization))
	for i, h := range auth.HeadersAuthorization {
		value, err := renderTemplate(h.HeaderTemplate, creds)
		if err != nil {
			return nil, nil, fmt.Errorf("headers_authorization[%d] (%s): %w", i, h.HeaderName, err)
		}
		headers[http.CanonicalHeaderKey(h.HeaderName)] = value
	}
	params = make(map[string]string, len(auth.ParamAuthorization))
	for i, p := range auth.ParamAuthorization {
		value, err := renderTemplate(p.ParamTemplate, creds)
		if err != nil {
			return nil, nil, fmt.Errorf("param_authorization[%d] (%s): %w", i, p.ParamName, err)
		}
		params[p.ParamName] = value
	}
	return headers, params, nil
}

// renderTemplate executes one template. An empty result is an error: a
// credential-bearing header must never go out blank, which is what a typo'd
// provider or field would otherwise produce.
func renderTemplate(text string, creds Credentials) (string, error) {
	tmpl, err := template.New("auth").
		Option("missingkey=error").
		Funcs(templateFuncs(creds)).
		Parse(text)
	if err != nil {
		return "", fmt.Errorf("template does not parse: %w%s", err, credHint)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, map[string]extidentities.Credential(creds)); err != nil {
		return "", fmt.Errorf("template does not render: %w%s", err, credHint)
	}
	value := out.String()
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("template rendered empty")
	}
	return value, nil
}

// credHint is appended to template errors: the field-selector form is the
// mistake people make first, and it fails at PARSE time with a message that
// does not explain itself.
const credHint = ` (provider names may contain '-' or '_', which a field ` +
	`selector cannot express — use {{ (cred "my-provider").Credentials }} ` +
	`rather than {{ .my-provider.Credentials }})`

// templateFuncs is the entire function surface a template gets.
func templateFuncs(creds Credentials) template.FuncMap {
	return template.FuncMap{
		// cred looks a provider up by name. Unlike the builtin index, it
		// ERRORS on an unknown provider instead of yielding a zero
		// Credential, so a typo cannot silently render a blank secret.
		"cred": func(name string) (extidentities.Credential, error) {
			c, ok := creds[name]
			if !ok {
				return extidentities.Credential{}, fmt.Errorf("no credential for provider %q; declare it in this tool's access requirements", name)
			}
			return c, nil
		},
		// b64 is what HTTP Basic actually needs:
		//   Basic {{ b64 (printf "%s:%s" (cred "jira").Username (cred "jira").Credentials) }}
		"b64": func(s string) string {
			return base64.StdEncoding.EncodeToString([]byte(s))
		},
	}
}

// MergeParams merges rendered credential params into the model's arguments.
// Credentials WIN over anything the model supplied: the model must never
// control a credential-bearing field. The returned names are the arguments
// that were overwritten — names only, for logging.
func MergeParams(arguments json.RawMessage, params map[string]string) (json.RawMessage, []string, error) {
	if len(params) == 0 {
		return arguments, nil, nil
	}
	fields := map[string]any{}
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, &fields); err != nil {
			return nil, nil, fmt.Errorf("tools: tool arguments are not a JSON object: %w", err)
		}
	}
	var overwritten []string
	for name, value := range params {
		if _, exists := fields[name]; exists {
			overwritten = append(overwritten, name)
		}
		fields[name] = value
	}
	sort.Strings(overwritten)
	merged, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, fmt.Errorf("tools: marshal merged arguments: %w", err)
	}
	return merged, overwritten, nil
}

// ValidateToolAuthorization checks a server's per-tool credential
// configuration. Every error is user-facing text naming the tool and the
// offending entry.
func ValidateToolAuthorization(reqs map[string][]AccessRequirement, auths map[string]ToolAuthorization) error {
	for tool, list := range reqs {
		if strings.TrimSpace(tool) == "" {
			return fmt.Errorf("tool_access_requirements has an empty tool name")
		}
		levels := make(map[string]string, len(list))
		for i, req := range list {
			if err := extidentities.ValidateProviderName(req.Provider); err != nil {
				return fmt.Errorf("tool %q: tool_access_requirements[%d]: %w", tool, i, err)
			}
			// An omitted level means "any credential from this provider" —
			// the only thing a scope-less credential can promise.
			if req.AccessLevel != "" {
				if err := extidentities.ValidateAccessLevelName(req.AccessLevel); err != nil {
					return fmt.Errorf("tool %q: tool_access_requirements[%d]: access_level %w", tool, i, err)
				}
			}
			// One access level per provider per tool: a tool needs one
			// credential from a provider, and two levels would leave which
			// one to fetch undefined.
			if previous, seen := levels[req.Provider]; seen {
				return fmt.Errorf("tool %q requires provider %q twice (access levels %q and %q); a tool may require one access level per provider",
					tool, req.Provider, previous, req.AccessLevel)
			}
			levels[req.Provider] = req.AccessLevel
		}
	}

	// Validate what each tool key EFFECTIVELY resolves to, not what it
	// literally declares. A tool with its own requirements inherits the
	// wildcard's authorization unless it declares one, so the pair that has
	// to render together is not always the pair that was written together.
	for _, tool := range effectiveToolKeys(reqs, auths) {
		auth, inherited := effectiveAuthorization(auths, tool)
		if len(auth.HeadersAuthorization) == 0 && len(auth.ParamAuthorization) == 0 {
			if _, declared := auths[tool]; declared {
				return fmt.Errorf("tool %q has an empty tool_authorizations entry; give it a header or a param", tool)
			}
			continue // requirements without an authorization: nothing to render
		}
		declared := effectiveRequirements(reqs, tool)
		if len(declared) == 0 {
			return fmt.Errorf("tool %q has tool_authorizations but no tool_access_requirements; there is no credential to render", tool)
		}
		// Name the inheritance in errors: a failure reported against a tool
		// whose own configuration looks fine is otherwise baffling.
		where := fmt.Sprintf("tool %q", tool)
		if inherited {
			where = fmt.Sprintf("tool %q (inheriting the %q authorization)", tool, WildcardTool)
		}
		seenHeaders := map[string]bool{}
		for i, h := range auth.HeadersAuthorization {
			if !headerNamePattern.MatchString(h.HeaderName) {
				return fmt.Errorf("%s: headers_authorization[%d]: header_name %q must be 1-64 characters of letters, digits or '-'", where, i, h.HeaderName)
			}
			canonical := http.CanonicalHeaderKey(h.HeaderName)
			if isReservedHeader(canonical) {
				return fmt.Errorf("%s: headers_authorization[%d]: header_name %q is reserved by the platform", where, i, h.HeaderName)
			}
			if seenHeaders[canonical] {
				return fmt.Errorf("%s: headers_authorization[%d]: header_name %q is set twice", where, i, h.HeaderName)
			}
			seenHeaders[canonical] = true
		}
		seenParams := map[string]bool{}
		for i, p := range auth.ParamAuthorization {
			if !paramNamePattern.MatchString(p.ParamName) {
				return fmt.Errorf("%s: param_authorization[%d]: param_name %q must be 1-64 characters of letters, digits, '_', '.' or '-'", where, i, p.ParamName)
			}
			if seenParams[p.ParamName] {
				return fmt.Errorf("%s: param_authorization[%d]: param_name %q is set twice", where, i, p.ParamName)
			}
			seenParams[p.ParamName] = true
		}
		// Render against sentinels: this proves the templates parse, that
		// every provider they name is declared, that every field exists on
		// Credential, and that nothing renders empty.
		if _, _, err := Render(auth, sentinelCredentials(declared)); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

// effectiveToolKeys lists every tool key either map mentions, sorted so the
// first error a caller sees does not depend on map iteration order.
func effectiveToolKeys(reqs map[string][]AccessRequirement, auths map[string]ToolAuthorization) []string {
	seen := make(map[string]bool, len(reqs)+len(auths))
	keys := make([]string, 0, len(reqs)+len(auths))
	for k := range reqs {
		seen[k] = true
		keys = append(keys, k)
	}
	for k := range auths {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// effectiveRequirements is Server.Requirements over bare maps, for
// validating a configuration before it is stored.
func effectiveRequirements(reqs map[string][]AccessRequirement, tool string) []AccessRequirement {
	if own, ok := reqs[tool]; ok {
		return own
	}
	if IsProbeKey(tool) {
		return nil
	}
	return reqs[WildcardTool]
}

// effectiveAuthorization is Server.Authorization over bare maps; the boolean
// reports that the entry came from the wildcard rather than the tool itself.
func effectiveAuthorization(auths map[string]ToolAuthorization, tool string) (ToolAuthorization, bool) {
	if own, ok := auths[tool]; ok {
		return own, false
	}
	if IsProbeKey(tool) {
		return ToolAuthorization{}, false
	}
	wildcard, inherited := auths[WildcardTool]
	return wildcard, inherited
}

// sentinelCredentials builds stand-in credentials for the declared
// providers, so validation exercises the real rendering path.
func sentinelCredentials(reqs []AccessRequirement) Credentials {
	creds := make(Credentials, len(reqs))
	for _, req := range reqs {
		creds[req.Provider] = extidentities.Credential{
			Credentials: "validation-credential",
			TokenType:   "bearer",
			Username:    "validation-user",
			Access:      []string{"validation-scope"},
			AccessLevel: req.AccessLevel,
		}
	}
	return creds
}

func isReservedHeader(canonical string) bool {
	if strings.HasPrefix(canonical, "X-Enact-") {
		return true
	}
	for _, h := range reservedHeaders {
		if strings.EqualFold(h, canonical) {
			return true
		}
	}
	return false
}

// EncodeAuthHeaders packs rendered headers into the envelope value.
func EncodeAuthHeaders(headers map[string]string) (string, error) {
	raw, err := json.Marshal(headers)
	if err != nil {
		return "", fmt.Errorf("tools: marshal tool auth headers: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > maxAuthEnvelopeBytes {
		return "", fmt.Errorf("tools: tool auth headers exceed %d bytes", maxAuthEnvelopeBytes)
	}
	return encoded, nil
}

// DecodeAuthHeaders unpacks an envelope value. It is defensive: the proxy
// routes accept anonymous callers, so this input is untrusted.
func DecodeAuthHeaders(encoded string) (map[string]string, error) {
	if len(encoded) > maxAuthEnvelopeBytes {
		return nil, fmt.Errorf("tools: tool auth envelope exceeds %d bytes", maxAuthEnvelopeBytes)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("tools: tool auth envelope is not valid base64url")
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil, fmt.Errorf("tools: tool auth envelope is not a JSON object")
	}
	for name := range headers {
		if !headerNamePattern.MatchString(name) {
			return nil, fmt.Errorf("tools: tool auth envelope carries an invalid header name %q", name)
		}
	}
	return headers, nil
}

// HeaderNames lists a header map's names, sorted — the logging-safe view of
// a credential set (ADR-0008: names, never values).
func HeaderNames(headers map[string]string) []string {
	out := make([]string, 0, len(headers))
	for name := range headers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
