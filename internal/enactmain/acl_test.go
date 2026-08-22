package enactmain

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// aclPath is the deployed access control list for this service. The test
// reaches out of the package deliberately: the file and the routes are two
// halves of one contract, and nothing else checks that they still agree.
const aclPath = "../../deploy/s2s/acl/enact-main.yaml"

var aclRoute = regexp.MustCompile(`route:\s*"([^"]+)"`)

// TestEveryRouteHasAnACLRule guards a failure mode that is invisible in
// development and total in production.
//
// The S2S filter is registered on every WebService of this service, and its
// ACL denies by default. A route added without a matching rule therefore
// works perfectly with S2S_ENABLED=false — the local default — and returns
// 403 for every caller, browser included, the moment it is deployed.
//
// This has already happened three times: conversation deletion, the provider
// routes when they moved off /admin, and the documentation endpoint.
func TestEveryRouteHasAnACLRule(t *testing.T) {
	allowed := map[string]bool{}
	raw, err := os.ReadFile(aclPath)
	if err != nil {
		t.Fatalf("read %s: %v", aclPath, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if m := aclRoute.FindStringSubmatch(line); m != nil {
			allowed[m[1]] = true
		}
	}
	if len(allowed) == 0 {
		t.Fatalf("no rules parsed from %s; the test would pass vacuously", aclPath)
	}

	// A zero-value API is enough: WebServices only registers routes, and the
	// handlers are taken as method values rather than called.
	registered := map[string]bool{}
	for _, ws := range (&MainAPI{}).WebServices() {
		for _, route := range ws.Routes() {
			key := route.Method + " " + strings.TrimSuffix(route.Path, "/")
			registered[key] = true
			if !allowed[key] {
				t.Errorf("route %q has no rule in %s; it will 403 for everyone once S2S is enabled", key, aclPath)
			}
		}
	}

	// Stale rules are the other half: when the provider routes moved off
	// /admin, the old rules stayed behind and quietly granted access to paths
	// that no longer exist. Harmless today, misleading forever.
	for rule := range allowed {
		if !registered[rule] {
			t.Errorf("rule %q in %s matches no registered route; remove it", rule, aclPath)
		}
	}
}
