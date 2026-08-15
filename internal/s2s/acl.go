package s2s

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// aclFile is the YAML schema of a service's ACL document. Route keys are
// "<METHOD> <route template>" exactly as go-restful reports them, e.g.
// "GET /v1/knowledge-bases/{id}". The allow list holds caller key ids;
// two special entries exist:
//
//   - "anonymous": requests without any service token
//   - "*": any caller presenting a VALID service token (never anonymous)
//
// A route with no rule denies everyone — default deny.
type aclFile struct {
	Service string `yaml:"service"`
	Rules   []struct {
		Route string   `yaml:"route"`
		Allow []string `yaml:"allow"`
	} `yaml:"rules"`
}

// acl maps route -> set of allowed callers.
type acl map[string]map[string]bool

func parseACL(doc string) (acl, error) {
	var f aclFile
	if err := yaml.Unmarshal([]byte(doc), &f); err != nil {
		return nil, fmt.Errorf("s2s: parse ACL yaml: %w", err)
	}
	rules := make(acl, len(f.Rules))
	for _, r := range f.Rules {
		if r.Route == "" {
			return nil, fmt.Errorf("s2s: ACL rule without route")
		}
		if _, dup := rules[r.Route]; dup {
			return nil, fmt.Errorf("s2s: ACL contains duplicate route %q", r.Route)
		}
		set := make(map[string]bool, len(r.Allow))
		for _, caller := range r.Allow {
			set[caller] = true
		}
		rules[r.Route] = set
	}
	return rules, nil
}

// allowed reports whether caller may access route. Default deny: an unlisted
// route admits no one. The "*" wildcard admits any authenticated caller but
// never anonymous.
func (a acl) allowed(route, caller string) bool {
	set, ok := a[route]
	if !ok {
		return false
	}
	if set[caller] {
		return true
	}
	return caller != Anonymous && set["*"]
}
