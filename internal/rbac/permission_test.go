package rbac

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		rule       string
		permission string
		want       bool
		why        string
	}{
		// Exact.
		{"enact:kb:edit:123", "enact:kb:edit:123", true, "identical"},
		{"enact:kb:edit:123", "enact:kb:edit:456", false, "different resource"},
		{"enact:kb:edit:123", "enact:kb:view:123", false, "different action"},
		{"enact:kb:edit:123", "enact:agent:edit:123", false, "different type"},

		// Wildcards, one segment each.
		{"enact:kb:view:*", "enact:kb:view:anything", true, "any id"},
		{"enact:kb:*:123", "enact:kb:delete:123", true, "any action on one resource"},
		{"enact:*:view:123", "enact:agent:view:123", true, "any type"},
		{"enact:kb:view:*", "enact:kb:edit:123", false, "wildcard does not widen the action"},

		// A short rule leaves the rest unconstrained.
		{"enact:kb", "enact:kb:edit:123", true, "type-wide"},
		{"enact:kb:*", "enact:kb:edit:123", true, "type-wide, spelled with a wildcard"},
		{"enact", "enact:kb:edit:123", true, "everything this platform issues"},
		{"enact:kb", "enact:agent:edit:123", false, "still bounded by the segments it names"},

		// A longer rule than the permission never matches: a narrow grant
		// must not be inherited by a broader question.
		{"enact:kb:edit:123", "enact:kb:edit", false, "rule is more specific than the question"},
		{"enact:kb:edit:123:extra", "enact:kb:edit:123", false, "rule has more segments"},

		// Degenerate input is a denial, never a grant.
		{"", "enact:kb:edit:123", false, "empty rule"},
		{"enact:kb:edit:123", "", false, "empty permission"},
		{"*", "enact:kb:edit:123", true, "a bare wildcard is a superuser rule"},
	}
	for _, c := range cases {
		if got := Match(c.rule, c.permission); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v — %s", c.rule, c.permission, got, c.want, c.why)
		}
	}
}

func TestAllows(t *testing.T) {
	// The example from the feature description: one role, full powers over
	// MCP servers and identity providers, and nothing else.
	rules := []string{"enact:mcp-server:*:*", "enact:provider:*:*"}

	for _, permission := range []string{
		"enact:mcp-server:edit:github-mcp",
		"enact:mcp-server:delete:anything",
		"enact:provider:create:gmail",
	} {
		if !Allows(rules, permission) {
			t.Errorf("Allows(%v, %q) = false, want true", rules, permission)
		}
	}
	for _, permission := range []string{
		"enact:kb:view:123",
		"enact:agent:use:abc",
		"enact:user:create:someone",
	} {
		if Allows(rules, permission) {
			t.Errorf("Allows(%v, %q) = true, want false — the role does not reach this", rules, permission)
		}
	}

	if Allows(nil, "enact:kb:view:123") {
		t.Error("a user with no rules was allowed something")
	}
}

func TestPermissionBuilder(t *testing.T) {
	if got := Permission(ResourceKB, ActionEdit, "123"); got != "enact:kb:edit:123" {
		t.Errorf("Permission() = %q", got)
	}
	// The builder and the matcher must agree, or a caller can be denied a
	// permission the rule was written for.
	if !Match("enact:kb:*:123", Permission(ResourceKB, ActionDelete, "123")) {
		t.Error("a rule written for a resource does not match the permission the builder produces")
	}
}
