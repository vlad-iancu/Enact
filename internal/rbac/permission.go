package rbac

import "strings"

// Permissions name one action on one resource, in four colon-separated
// segments:
//
//	enact:<resource-type>:<action>:<resource-id>
//	enact:kb:edit:9f3a…
//
// A RULE is written in the same grammar and may generalize two ways: `*`
// stands for any one segment, and a rule may stop early — `enact:kb:*` grants
// every action on every knowledge base, because segments the rule does not
// mention are unconstrained.
//
// Deliberately NOT regular expressions. `*` is not valid regex for what it
// obviously means here, every rule anyone would write is a glob, and a
// pattern language where a typo silently grants more than intended is the
// wrong tool for an authorization decision.
const (
	// Wildcard matches exactly one segment.
	Wildcard = "*"
	// Separator divides a permission into segments.
	Separator = ":"
	// Namespace is the first segment of every permission this platform issues.
	Namespace = "enact"
)

// Resource types. A permission's second segment.
const (
	ResourceKB           = "kb"
	ResourceAgent        = "agent"
	ResourceMCPServer    = "mcp-server"
	ResourceProvider     = "provider"
	ResourceIdentity     = "identity"
	ResourceConversation = "conversation"
	ResourceUser         = "user"
	ResourceRole         = "role"
	ResourceOrganization = "organization"
)

// Actions. A permission's third segment.
const (
	ActionView   = "view"
	ActionEdit   = "edit"
	ActionDelete = "delete"
	ActionCreate = "create"
	ActionUse    = "use"
)

// Permission builds a permission string, so callers do not hand-assemble
// colons and no typo silently asks for something nobody grants.
func Permission(resource, action, id string) string {
	return strings.Join([]string{Namespace, resource, action, id}, Separator)
}

// Match reports whether one rule grants one permission.
//
// The rule is read segment by segment: a literal must be equal, `*` matches
// anything, and running out of rule means the rest is unconstrained. Running
// out of PERMISSION does not: a rule for `enact:kb:edit:123` does not grant
// the broader `enact:kb:edit`, or a shorter permission would inherit a
// narrower rule's authority.
func Match(rule, permission string) bool {
	if rule == "" || permission == "" {
		return false
	}
	ruleSegments := strings.Split(rule, Separator)
	permSegments := strings.Split(permission, Separator)
	if len(ruleSegments) > len(permSegments) {
		return false
	}
	for i, segment := range ruleSegments {
		if segment == Wildcard {
			continue
		}
		if segment != permSegments[i] {
			return false
		}
	}
	return true
}

// Allows reports whether any rule in the set grants the permission. Rules are
// purely additive: there are no deny rules, so a grant cannot be taken away
// by adding another rule — only by removing the one that granted it.
func Allows(rules []string, permission string) bool {
	for _, rule := range rules {
		if Match(rule, permission) {
			return true
		}
	}
	return false
}
