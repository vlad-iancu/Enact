// Package rbac holds the organizations and role-based access control
// domain: who belongs to which organization, which roles exist inside it,
// what those roles grant, and the matcher that answers "may this user do
// this?".
//
// It is a standalone package because every service depends on it — ADR-0004:
// callers depend on the domain, never on another service's internals. The
// service that owns the data is enact-rbac; everything else reaches it
// through Client and evaluates with Allows.
package rbac

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Config holds the OpenSearch index names for the RBAC domain.
type Config struct {
	OrganizationsIndex string `env:"OPENSEARCH_INDEX_ORGANIZATIONS, default=enact-organizations"`
	RequestsIndex      string `env:"OPENSEARCH_INDEX_ORGANIZATION_REQUESTS, default=enact-organization-requests"`
	MembershipsIndex   string `env:"OPENSEARCH_INDEX_MEMBERSHIPS, default=enact-memberships"`
	RolesIndex         string `env:"OPENSEARCH_INDEX_ROLES, default=enact-roles"`
}

// Statuses of an organization request.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// DefaultOrganizationID is the organization every pre-existing user and
// resource is migrated into. It is an ordinary organization in every respect;
// only its id is fixed, so the migration is idempotent.
const DefaultOrganizationID = "default"

// Organization is the isolation boundary. Every user belongs to exactly one,
// every resource is created inside one, and nothing crosses.
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// CreatedBy is the user whose request was approved, or empty for
	// organizations the platform created (the default one).
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// OrganizationRequest is a user asking for an organization to exist. Only the
// platform administrator can approve one — otherwise anyone could mint an
// isolation boundary and populate it.
type OrganizationRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// RequestedBy is the user who asked; on approval they become its first
	// owner.
	RequestedBy      string `json:"requested_by"`
	RequestedByEmail string `json:"requested_by_email,omitempty"`
	Justification    string `json:"justification,omitempty"`
	Status           string `json:"status"`
	// DecidedBy/DecidedAt/Reason record the administrator's answer.
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	// OrganizationID is set once the request is approved and the
	// organization exists.
	OrganizationID string    `json:"organization_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Membership places one user in one organization. The document id is the
// user id, so "a user belongs to exactly one organization" is structural
// rather than a rule someone has to remember to enforce.
type Membership struct {
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
	// Owner may manage the organization: its members, its roles and their
	// rules, and who else is an owner.
	Owner     bool      `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Role is a named set of rules inside one organization, held by zero or more
// of its members. The same name may exist in many organizations; the document
// id is scoped (RoleDocID), so they never collide.
type Role struct {
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	// Rules are permission patterns (see permission.go). Purely additive.
	Rules []string `json:"rules"`
	// Members are the user ids holding this role.
	Members []string `json:"members"`
	// Hidden marks the per-user role that carries ownership of the resources
	// a user creates. Its name IS the user id, it has exactly one member, and
	// it is excluded from role listings — it is bookkeeping, not something an
	// owner assigns.
	Hidden      bool      `json:"hidden,omitempty"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Effective is what a service needs to answer any authorization question
// about one user: which organization they are in, whether they own it, and
// every rule they hold. Services cache this briefly and evaluate locally with
// Allows, so a permission check is not a network round trip.
type Effective struct {
	UserID         string   `json:"user_id"`
	OrganizationID string   `json:"organization_id"`
	Owner          bool     `json:"owner"`
	Rules          []string `json:"rules"`
	// Roles names the roles the rules came from, so a profile can show
	// "you hold these roles" rather than only the flattened grammar. The
	// hidden per-user ownership role is excluded: it is how ownership is
	// stored, not a role anyone was given.
	Roles []string `json:"roles,omitempty"`
}

// Allows reports whether this user may perform the permission.
//
// An organization OWNER may do anything inside their organization, without a
// rule saying so. Not a shortcut: an owner edits the roles and their rules,
// so any permission they lack they can grant themselves in one call.
// Enforcing rules against them would only add a step, while making the
// platform unusable the moment an organization is created — its first owner
// would hold no rules at all and could create nothing.
//
// Ordinary members hold exactly what their roles grant, which is what makes
// "access is gatekept by roles" true for everyone the owner adds.
func (e Effective) Allows(permission string) bool {
	if e.Owner {
		return true
	}
	return Allows(e.Rules, permission)
}

// RoleDocID scopes a role name to its organization, so "admin" in one
// organization is a different document from "admin" in another.
func RoleDocID(organizationID, name string) string {
	return organizationID + Separator + name
}

// namePattern constrains organization and role names: short, printable, and
// safe in a document id that uses ":" as its separator.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 _-]{0,63}$`)

// ValidateName checks an organization or role name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("name must be 1-64 characters: letters, digits, spaces, '-' or '_', starting alphanumeric")
	}
	return nil
}

// rulePattern constrains a rule's characters. The structure (segment count,
// wildcards) is the matcher's business; this only keeps a rule from carrying
// whitespace or characters that would make it unreadable in a listing.
var rulePattern = regexp.MustCompile(`^[a-zA-Z0-9:_*.-]+$`)

// ValidateRule checks one rule of a role.
//
// A rule must name the platform namespace or be the bare wildcard: without
// that, a typo like "kb:*" would silently grant nothing, and an operator
// would have no way to tell a useless rule from a working one.
func ValidateRule(rule string) error {
	if !rulePattern.MatchString(rule) {
		return fmt.Errorf("rule %q may only contain letters, digits, ':', '_', '.', '-' or '*'", rule)
	}
	if strings.Count(rule, Separator) > 3 {
		return fmt.Errorf("rule %q has more than four segments; the grammar is %s:<type>:<action>:<id>", rule, Namespace)
	}
	first, _, _ := strings.Cut(rule, Separator)
	if first != Namespace && first != Wildcard {
		return fmt.Errorf("rule %q must start with %q (or be %q); it would never match anything", rule, Namespace, Wildcard)
	}
	return nil
}

// ValidateRules checks every rule of a role.
func ValidateRules(rules []string) error {
	for i, rule := range rules {
		if err := ValidateRule(rule); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

// OwnerRules are the rules granted to a user over a resource they just
// created: everything, on that resource alone.
func OwnerRules(resource, id string) []string {
	return []string{Namespace + Separator + resource + Separator + Wildcard + Separator + id}
}
