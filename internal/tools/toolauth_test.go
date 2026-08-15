package tools

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"enact/internal/extidentities"
)

func testCreds() Credentials {
	return Credentials{
		"github": {Credentials: "ghp_token", TokenType: "bearer", AccessLevel: "read"},
		"jira-cloud": {
			Credentials: "api-token", TokenType: "basic", Username: "a@b.c",
		},
	}
}

func TestRenderHeadersAndParams(t *testing.T) {
	auth := ToolAuthorization{
		HeadersAuthorization: []HeaderAuthorization{
			{HeaderName: "Authorization", HeaderTemplate: `Bearer {{ .github.Credentials }}`},
			// The hyphenated provider is only reachable through cred.
			{HeaderName: "X-Jira-Auth", HeaderTemplate: `Basic {{ b64 (printf "%s:%s" (cred "jira-cloud").Username (cred "jira-cloud").Credentials) }}`},
		},
		ParamAuthorization: []ParamAuthorization{
			{ParamName: "api_key", ParamTemplate: `{{ .github.Credentials }}`},
		},
	}
	headers, params, err := Render(auth, testCreds())
	if err != nil {
		t.Fatal(err)
	}
	if headers["Authorization"] != "Bearer ghp_token" {
		t.Errorf("Authorization = %q", headers["Authorization"])
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("a@b.c:api-token"))
	if headers["X-Jira-Auth"] != wantBasic {
		t.Errorf("X-Jira-Auth = %q, want %q", headers["X-Jira-Auth"], wantBasic)
	}
	if params["api_key"] != "ghp_token" {
		t.Errorf("api_key = %q", params["api_key"])
	}
}

func TestRenderRejectsUnknownProviderAndEmptyResult(t *testing.T) {
	// cred errors on an undeclared provider rather than yielding a zero
	// Credential — the whole reason it exists instead of index.
	_, _, err := Render(ToolAuthorization{HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "X-A", HeaderTemplate: `{{ (cred "nope").Credentials }}`},
	}}, testCreds())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown provider: err = %v, want it named", err)
	}

	// A credential header must never go out blank.
	_, _, err = Render(ToolAuthorization{HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "X-A", HeaderTemplate: `{{ .github.Username }}`},
	}}, testCreds())
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty render: err = %v, want an 'empty' error", err)
	}
}

func TestRenderFieldSelectorOnHyphenNameExplainsCred(t *testing.T) {
	// {{ .jira-cloud.X }} is a PARSE error in Go templates; the message must
	// point at the cred idiom or nobody can guess the fix.
	_, _, err := Render(ToolAuthorization{HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "X-A", HeaderTemplate: `{{ .jira-cloud.Credentials }}`},
	}}, testCreds())
	if err == nil {
		t.Fatal("field selector on a hyphenated provider was accepted")
	}
	if !strings.Contains(err.Error(), `cred "my-provider"`) {
		t.Errorf("err = %v, want it to spell out the cred idiom", err)
	}
}

func TestMergeParamsCredentialsWin(t *testing.T) {
	merged, overwritten, err := MergeParams(
		json.RawMessage(`{"message":"hi","api_key":"model-supplied"}`),
		map[string]string{"api_key": "real-secret"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatal(err)
	}
	if out["api_key"] != "real-secret" {
		t.Errorf("api_key = %v, want the credential to win over the model", out["api_key"])
	}
	if out["message"] != "hi" {
		t.Errorf("model arguments were lost: %v", out)
	}
	if len(overwritten) != 1 || overwritten[0] != "api_key" {
		t.Errorf("overwritten = %v, want [api_key]", overwritten)
	}

	// Absent/null arguments are a valid starting point.
	merged, _, err = MergeParams(nil, map[string]string{"api_key": "x"})
	if err != nil || !strings.Contains(string(merged), "api_key") {
		t.Errorf("merge into empty arguments: %s, %v", merged, err)
	}
	// No params: arguments pass through untouched.
	if merged, _, _ := MergeParams(json.RawMessage(`{"a":1}`), nil); string(merged) != `{"a":1}` {
		t.Errorf("no-op merge changed the arguments: %s", merged)
	}
}

func TestWildcardAppliesToEveryTool(t *testing.T) {
	bearer := ToolAuthorization{HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "Authorization", HeaderTemplate: `Bearer {{ (cred "github").Credentials }}`},
	}}
	apiKey := ToolAuthorization{ParamAuthorization: []ParamAuthorization{
		{ParamName: "api_key", ParamTemplate: `{{ (cred "github").Credentials }}`},
	}}
	server := Server{
		ToolAccessRequirements: map[string][]AccessRequirement{
			WildcardTool: {{Provider: "github", AccessLevel: "read"}},
			"admin_op":   {{Provider: "github", AccessLevel: "admin"}},
		},
		ToolAuthorizations: map[string]ToolAuthorization{
			WildcardTool: bearer,
			"odd_one":    apiKey,
		},
	}

	// A tool nobody configured gets the wildcard, for both maps.
	if reqs := server.Requirements("list_issues"); len(reqs) != 1 || reqs[0].AccessLevel != "read" {
		t.Errorf("unconfigured tool requirements = %+v, want the wildcard's github:read", reqs)
	}
	if auth, ok := server.Authorization("list_issues"); !ok || len(auth.HeadersAuthorization) != 1 {
		t.Errorf("unconfigured tool authorization = %+v (ok=%v), want the wildcard's header", auth, ok)
	}

	// A tool's own entry wins outright — and only for the map it appears in.
	if reqs := server.Requirements("admin_op"); len(reqs) != 1 || reqs[0].AccessLevel != "admin" {
		t.Errorf("admin_op requirements = %+v, want its own github:admin, not the wildcard's", reqs)
	}
	if auth, _ := server.Authorization("admin_op"); len(auth.HeadersAuthorization) != 1 {
		t.Errorf("admin_op declares no authorization, so it must inherit the wildcard's header: %+v", auth)
	}
	if auth, _ := server.Authorization("odd_one"); len(auth.ParamAuthorization) != 1 || len(auth.HeadersAuthorization) != 0 {
		t.Errorf("odd_one authorization = %+v, want its own param and NOT the wildcard's header", auth)
	}
	if reqs := server.Requirements("odd_one"); len(reqs) != 1 || reqs[0].AccessLevel != "read" {
		t.Errorf("odd_one declares no requirements, so it must inherit the wildcard's: %+v", reqs)
	}

	// A server with no wildcard is unaffected.
	plain := Server{ToolAccessRequirements: map[string][]AccessRequirement{"t": {{Provider: "github", AccessLevel: "read"}}}}
	if reqs := plain.Requirements("other"); len(reqs) != 0 {
		t.Errorf("without a wildcard an unconfigured tool must need nothing, got %+v", reqs)
	}
	if err := ValidateToolAuthorization(server.ToolAccessRequirements, server.ToolAuthorizations); err != nil {
		t.Errorf("valid wildcard configuration rejected: %v", err)
	}
}

func TestAccessLevelIsOptional(t *testing.T) {
	// A credential with nothing to say about scope — an API key that either
	// works or does not — is expressed by naming no level.
	reqs := map[string][]AccessRequirement{"t": {{Provider: "gatekeeper"}}}
	auths := map[string]ToolAuthorization{"t": {HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "X-Gate", HeaderTemplate: `{{ (cred "gatekeeper").Credentials }}`},
	}}}
	if err := ValidateToolAuthorization(reqs, auths); err != nil {
		t.Fatalf("a requirement without an access level was rejected: %v", err)
	}
	// And it must ask the identity service for NO required access, rather
	// than for coverage of an empty scope nobody holds.
	if got := (AccessRequirement{Provider: "gatekeeper"}).RequiredAccess(); got != nil {
		t.Errorf("RequiredAccess() = %v, want nil when no level is named", got)
	}
	if got := (AccessRequirement{Provider: "github", AccessLevel: "read"}).RequiredAccess(); len(got) != 1 || got[0] != "read" {
		t.Errorf("RequiredAccess() = %v, want [read]", got)
	}
	// A malformed level is still rejected; only absence is allowed.
	if err := ValidateToolAuthorization(map[string][]AccessRequirement{"t": {{Provider: "github", AccessLevel: "Read"}}}, nil); err == nil {
		t.Error("a malformed access level was accepted")
	}
}

func TestProbeKeysDoNotInheritTheWildcard(t *testing.T) {
	bearer := ToolAuthorization{HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "Authorization", HeaderTemplate: `Bearer {{ (cred "github").Credentials }}`},
	}}
	gate := ToolAuthorization{HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "X-Gate", HeaderTemplate: `{{ (cred "gatekeeper").Credentials }}`},
	}}

	// A wildcard covers tools. It must NOT quietly start spending the
	// owner's credential on the platform's own probes.
	wildcardOnly := Server{
		ToolAccessRequirements: map[string][]AccessRequirement{WildcardTool: {{Provider: "github", AccessLevel: "read"}}},
		ToolAuthorizations:     map[string]ToolAuthorization{WildcardTool: bearer},
	}
	if _, _, configured := wildcardOnly.Probe(ProbeInitialize); configured {
		t.Error("the wildcard configured a probe; probe keys must be declared outright")
	}

	// Declaring one phase covers both: a server that gates its handshake
	// gates everything after it.
	gated := Server{
		ToolAccessRequirements: map[string][]AccessRequirement{ProbeInitialize: {{Provider: "gatekeeper", AccessLevel: "read"}}},
		ToolAuthorizations:     map[string]ToolAuthorization{ProbeInitialize: gate},
	}
	for _, phase := range []string{ProbeInitialize, ProbeListTools} {
		reqs, auth, configured := gated.Probe(phase)
		if !configured || len(reqs) != 1 || reqs[0].Provider != "gatekeeper" || len(auth.HeadersAuthorization) != 1 {
			t.Errorf("Probe(%q) = %+v/%+v (configured=%v), want the declared gate for both phases", phase, reqs, auth, configured)
		}
	}
	// And a probe key never leaks into what a TOOL sends.
	if reqs := gated.Requirements("secret_number"); len(reqs) != 0 {
		t.Errorf("a tool inherited the probe requirements: %+v", reqs)
	}
	if !IsProbeKey(ProbeInitialize) || !IsProbeKey(ProbeListTools) || IsProbeKey("secret_number") || IsProbeKey(WildcardTool) {
		t.Error("IsProbeKey does not identify exactly the two probe keys")
	}
	if err := ValidateToolAuthorization(gated.ToolAccessRequirements, gated.ToolAuthorizations); err != nil {
		t.Errorf("a valid probe configuration was rejected: %v", err)
	}
}

func TestValidateToolAuthorizationWildcard(t *testing.T) {
	// The inherited authorization must render against the INHERITING tool's
	// own requirements, or the call fails at run time with the credential
	// already fetched.
	err := ValidateToolAuthorization(
		map[string][]AccessRequirement{
			WildcardTool: {{Provider: "github", AccessLevel: "read"}},
			"jira_sync":  {{Provider: "jira", AccessLevel: "write"}},
		},
		map[string]ToolAuthorization{WildcardTool: {HeadersAuthorization: []HeaderAuthorization{
			{HeaderName: "Authorization", HeaderTemplate: `Bearer {{ (cred "github").Credentials }}`},
		}}},
	)
	if err == nil {
		t.Fatal("a tool whose own requirements do not cover the inherited authorization was accepted")
	}
	if !strings.Contains(err.Error(), "jira_sync") || !strings.Contains(err.Error(), WildcardTool) {
		t.Errorf("err = %v, want it to name both the tool and the wildcard it inherited from", err)
	}

	// A wildcard authorization with no requirements anywhere has nothing to
	// render.
	err = ValidateToolAuthorization(nil, map[string]ToolAuthorization{WildcardTool: {HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "X-A", HeaderTemplate: `{{ (cred "github").Credentials }}`},
	}}})
	if err == nil || !strings.Contains(err.Error(), "no tool_access_requirements") {
		t.Errorf("wildcard authorization without requirements: err = %v", err)
	}

	// Requirements without any authorization stay legal (the credential is
	// fetched but not sent — validated elsewhere, not an error here).
	if err := ValidateToolAuthorization(
		map[string][]AccessRequirement{WildcardTool: {{Provider: "github", AccessLevel: "read"}}}, nil); err != nil {
		t.Errorf("wildcard requirements without authorizations rejected: %v", err)
	}
}

func TestValidateToolAuthorization(t *testing.T) {
	valid := map[string][]AccessRequirement{"t": {{Provider: "github", AccessLevel: "read"}}}
	validAuth := map[string]ToolAuthorization{"t": {HeadersAuthorization: []HeaderAuthorization{
		{HeaderName: "Authorization", HeaderTemplate: `Bearer {{ (cred "github").Credentials }}`},
	}}}
	if err := ValidateToolAuthorization(valid, validAuth); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}

	cases := map[string]struct {
		reqs  map[string][]AccessRequirement
		auths map[string]ToolAuthorization
		want  string
	}{
		"one level per provider": {
			reqs: map[string][]AccessRequirement{"t": {
				{Provider: "github", AccessLevel: "read"},
				{Provider: "github", AccessLevel: "write"},
			}},
			want: "twice",
		},
		"authorization without requirement": {
			auths: validAuth,
			want:  "no tool_access_requirements",
		},
		"template names an undeclared provider": {
			reqs: valid,
			auths: map[string]ToolAuthorization{"t": {HeadersAuthorization: []HeaderAuthorization{
				{HeaderName: "X-A", HeaderTemplate: `{{ (cred "gitlab").Credentials }}`},
			}}},
			want: "gitlab",
		},
		"reserved header": {
			reqs: valid,
			auths: map[string]ToolAuthorization{"t": {HeadersAuthorization: []HeaderAuthorization{
				{HeaderName: "X-Enact-Tool-Auth", HeaderTemplate: `{{ (cred "github").Credentials }}`},
			}}},
			want: "reserved",
		},
		"duplicate header": {
			reqs: valid,
			auths: map[string]ToolAuthorization{"t": {HeadersAuthorization: []HeaderAuthorization{
				{HeaderName: "X-A", HeaderTemplate: `{{ (cred "github").Credentials }}`},
				{HeaderName: "x-a", HeaderTemplate: `{{ (cred "github").Credentials }}`},
			}}},
			want: "twice",
		},
		"empty authorization entry": {
			reqs:  valid,
			auths: map[string]ToolAuthorization{"t": {}},
			want:  "empty",
		},
		"bad provider name": {
			reqs: map[string][]AccessRequirement{"t": {{Provider: "GitHub", AccessLevel: "read"}}},
			want: "lowercase",
		},
		"bad access level": {
			reqs: map[string][]AccessRequirement{"t": {{Provider: "github", AccessLevel: "Read"}}},
			want: "access_level",
		},
	}
	for name, tc := range cases {
		err := ValidateToolAuthorization(tc.reqs, tc.auths)
		if err == nil {
			t.Errorf("%s: accepted, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestValidationProvesRendering(t *testing.T) {
	// Validation renders through the same code path as a real call, so a
	// configuration that validates cannot fail to render later.
	reqs := map[string][]AccessRequirement{"t": {{Provider: "my-provider", AccessLevel: "read"}}}
	auths := map[string]ToolAuthorization{"t": {ParamAuthorization: []ParamAuthorization{
		{ParamName: "token", ParamTemplate: `{{ (cred "my-provider").Credentials }}`},
	}}}
	if err := ValidateToolAuthorization(reqs, auths); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	_, params, err := Render(auths["t"], Credentials{
		"my-provider": extidentities.Credential{Credentials: "real"},
	})
	if err != nil || params["token"] != "real" {
		t.Errorf("render after validation: %v, %v", params, err)
	}
}

func TestAuthHeaderEnvelopeRoundTrip(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer ghp_x", "X-Api-Key": "k"}
	encoded, err := EncodeAuthHeaders(headers)
	if err != nil {
		t.Fatal(err)
	}
	// The envelope must be opaque: a proxy log or an intermediary must not
	// show the credential.
	if strings.Contains(encoded, "ghp_x") {
		t.Error("the envelope leaks the credential in plain text")
	}
	decoded, err := DecodeAuthHeaders(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded["Authorization"] != "Bearer ghp_x" || decoded["X-Api-Key"] != "k" {
		t.Errorf("decoded = %v", decoded)
	}

	// Untrusted input: the proxy routes accept anonymous callers.
	for name, bad := range map[string]string{
		"not base64":    "!!!!",
		"not an object": base64.RawURLEncoding.EncodeToString([]byte(`["a"]`)),
		"bad header":    base64.RawURLEncoding.EncodeToString([]byte(`{"Bad Header":"v"}`)),
		"oversized":     strings.Repeat("A", maxAuthEnvelopeBytes+1),
	} {
		if _, err := DecodeAuthHeaders(bad); err == nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
}
