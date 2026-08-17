package rbac

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"enact/internal/opensearch"
)

// Repository persists the RBAC domain in OpenSearch across four indices:
// organizations, the requests to create them, memberships, and roles.
type Repository struct {
	os            *opensearch.Client
	organizations string
	requests      string
	memberships   string
	roles         string
}

// NewRepository returns a Repository over the indices in cfg.
func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{
		os:            os,
		organizations: cfg.OrganizationsIndex,
		requests:      cfg.RequestsIndex,
		memberships:   cfg.MembershipsIndex,
		roles:         cfg.RolesIndex,
	}
}

// EnsureIndices verifies every index exists; they are created by
// infrastructure provisioning, never by services.
func (r *Repository) EnsureIndices(ctx context.Context) error {
	for _, index := range []string{r.organizations, r.requests, r.memberships, r.roles} {
		exists, err := r.os.IndexExists(ctx, index)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("rbac: required index %q is missing; run `make infrastructure-up` to create it", index)
		}
	}
	return nil
}

// --- organizations ---------------------------------------------------------

// SaveOrganization creates or replaces an organization.
func (r *Repository) SaveOrganization(ctx context.Context, org Organization) error {
	body, err := json.Marshal(org)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.organizations, org.ID, body)
}

// GetOrganization fetches one by id; the boolean reports existence.
func (r *Repository) GetOrganization(ctx context.Context, id string) (Organization, bool, error) {
	var org Organization
	found, err := r.os.GetSource(ctx, r.organizations, id, &org)
	return org, found, err
}

// ListOrganizations returns every organization, newest first. A
// platform-administrator view: nobody else may see across organizations.
func (r *Repository) ListOrganizations(ctx context.Context) ([]Organization, error) {
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"match_all": map[string]any{}},
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, err
	}
	hits, err := r.os.Search(ctx, r.organizations, body)
	if err != nil {
		return nil, err
	}
	out := make([]Organization, 0, len(hits))
	for _, h := range hits {
		var org Organization
		if err := json.Unmarshal(h.Source, &org); err != nil {
			return nil, err
		}
		out = append(out, org)
	}
	return out, nil
}

// DeleteOrganization removes an organization record. Callers are responsible
// for its members and roles.
func (r *Repository) DeleteOrganization(ctx context.Context, id string) error {
	return r.os.DeleteDoc(ctx, r.organizations, id)
}

// --- organization requests -------------------------------------------------

// SaveRequest creates or replaces an organization request.
func (r *Repository) SaveRequest(ctx context.Context, req OrganizationRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.requests, req.ID, body)
}

// GetRequest fetches one request by id.
func (r *Repository) GetRequest(ctx context.Context, id string) (OrganizationRequest, bool, error) {
	var req OrganizationRequest
	found, err := r.os.GetSource(ctx, r.requests, id, &req)
	return req, found, err
}

// ListRequests returns requests, optionally filtered by status and by the
// user who made them.
func (r *Repository) ListRequests(ctx context.Context, status, requestedBy string) ([]OrganizationRequest, error) {
	var filters []any
	for field, value := range map[string]string{"status": status, "requested_by": requestedBy} {
		if value != "" {
			filters = append(filters, map[string]any{"term": map[string]any{field: value}})
		}
	}
	query := map[string]any{"match_all": map[string]any{}}
	if len(filters) > 0 {
		query = map[string]any{"bool": map[string]any{"filter": filters}}
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": query,
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, err
	}
	hits, err := r.os.Search(ctx, r.requests, body)
	if err != nil {
		return nil, err
	}
	out := make([]OrganizationRequest, 0, len(hits))
	for _, h := range hits {
		var req OrganizationRequest
		if err := json.Unmarshal(h.Source, &req); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, nil
}

// --- memberships -----------------------------------------------------------

// SaveMembership places a user in an organization. The document id is the
// user id, so a second call moves them rather than adding a membership.
func (r *Repository) SaveMembership(ctx context.Context, m Membership) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.memberships, m.UserID, body)
}

// GetMembership returns the user's organization; the boolean is false for a
// user who belongs to none — which is every self-registered user until an
// organization request of theirs is approved.
func (r *Repository) GetMembership(ctx context.Context, userID string) (Membership, bool, error) {
	var m Membership
	found, err := r.os.GetSource(ctx, r.memberships, userID, &m)
	return m, found, err
}

// DeleteMembership removes a user from their organization.
func (r *Repository) DeleteMembership(ctx context.Context, userID string) error {
	return r.os.DeleteDoc(ctx, r.memberships, userID)
}

// ListMembers returns every membership of one organization.
func (r *Repository) ListMembers(ctx context.Context, organizationID string) ([]Membership, error) {
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"organization_id": organizationID}},
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "asc"}}},
	})
	if err != nil {
		return nil, err
	}
	hits, err := r.os.Search(ctx, r.memberships, body)
	if err != nil {
		return nil, err
	}
	out := make([]Membership, 0, len(hits))
	for _, h := range hits {
		var m Membership
		if err := json.Unmarshal(h.Source, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// --- roles -----------------------------------------------------------------

// SaveRole creates or replaces a role.
func (r *Repository) SaveRole(ctx context.Context, role Role) error {
	body, err := json.Marshal(role)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.roles, RoleDocID(role.OrganizationID, role.Name), body)
}

// GetRole fetches one role of one organization.
func (r *Repository) GetRole(ctx context.Context, organizationID, name string) (Role, bool, error) {
	var role Role
	found, err := r.os.GetSource(ctx, r.roles, RoleDocID(organizationID, name), &role)
	return role, found, err
}

// DeleteRole removes a role.
func (r *Repository) DeleteRole(ctx context.Context, organizationID, name string) error {
	return r.os.DeleteDoc(ctx, r.roles, RoleDocID(organizationID, name))
}

// ListRoles returns an organization's roles. Hidden roles — the per-user
// ownership bookkeeping — are excluded unless asked for, because they are not
// something an owner assigns or edits.
func (r *Repository) ListRoles(ctx context.Context, organizationID string, includeHidden bool) ([]Role, error) {
	filters := []any{map[string]any{"term": map[string]any{"organization_id": organizationID}}}
	query := map[string]any{"bool": map[string]any{"filter": filters}}
	if !includeHidden {
		query = map[string]any{"bool": map[string]any{
			"filter":   filters,
			"must_not": []any{map[string]any{"term": map[string]any{"hidden": true}}},
		}}
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": query,
		"sort":  []any{map[string]any{"name": map[string]any{"order": "asc"}}},
	})
	if err != nil {
		return nil, err
	}
	return r.searchRoles(ctx, body)
}

// RolesOf returns every role one user holds, across the whole index. The
// user's own organization is whatever their membership says; a role from
// another organization cannot be held, because assignment checks membership.
func (r *Repository) RolesOf(ctx context.Context, userID string) ([]Role, error) {
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"members": userID}},
	})
	if err != nil {
		return nil, err
	}
	return r.searchRoles(ctx, body)
}

func (r *Repository) searchRoles(ctx context.Context, body []byte) ([]Role, error) {
	hits, err := r.os.Search(ctx, r.roles, body)
	if err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(hits))
	for _, h := range hits {
		var role Role
		if err := json.Unmarshal(h.Source, &role); err != nil {
			return nil, err
		}
		out = append(out, role)
	}
	return out, nil
}

// DeleteRolesOfOrganization removes every role of an organization.
func (r *Repository) DeleteRolesOfOrganization(ctx context.Context, organizationID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"organization_id": organizationID}},
	})
	if err != nil {
		return err
	}
	return r.os.DeleteByQuery(ctx, r.roles, body)
}

// Effective assembles everything an authorization decision needs about one
// user: their organization, whether they own it, and every rule they hold —
// their assigned roles plus the hidden role carrying what they own.
//
// A user with no membership gets an Effective with no organization and no
// rules, which denies everything by construction.
func (r *Repository) Effective(ctx context.Context, userID string) (Effective, error) {
	out := Effective{UserID: userID}
	membership, found, err := r.GetMembership(ctx, userID)
	if err != nil {
		return Effective{}, err
	}
	if !found {
		return out, nil
	}
	out.OrganizationID = membership.OrganizationID
	out.Owner = membership.Owner

	roles, err := r.RolesOf(ctx, userID)
	if err != nil {
		return Effective{}, err
	}
	seen := map[string]bool{}
	for _, role := range roles {
		// A role from another organization cannot grant anything here: the
		// user acts inside their own organization only.
		if role.OrganizationID != membership.OrganizationID {
			continue
		}
		if !role.Hidden {
			out.Roles = append(out.Roles, role.Name)
		}
		for _, rule := range role.Rules {
			if !seen[rule] {
				seen[rule] = true
				out.Rules = append(out.Rules, rule)
			}
		}
	}
	sort.Strings(out.Rules)
	sort.Strings(out.Roles)
	return out, nil
}

// MyRoles returns the caller's own roles in full, hidden ones excluded — what
// a profile page shows. Unlike ListRoles this needs no ownership: a user may
// always see which roles they themselves hold and what those roles grant.
func (r *Repository) MyRoles(ctx context.Context, userID string) ([]Role, error) {
	membership, found, err := r.GetMembership(ctx, userID)
	if err != nil || !found {
		return nil, err
	}
	roles, err := r.RolesOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(roles))
	for _, role := range roles {
		if role.Hidden || role.OrganizationID != membership.OrganizationID {
			continue
		}
		out = append(out, role)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Grant adds rules to a user's hidden ownership role, creating it if needed.
// This is how "the user who created a resource owns it" is expressed.
//
// Applied as ONE atomic scripted update rather than get-append-index. Two
// resources created at the same moment by the same user are two concurrent
// grants against the same document, and a read-modify-write pair loses one of
// them — leaving a resource its creator cannot open, intermittently and
// invisibly.
func (r *Repository) Grant(ctx context.Context, organizationID, userID string, rules []string) error {
	return r.changeRules(ctx, organizationID, userID, rules, true)
}

// Revoke removes rules from a user's hidden ownership role — used when the
// resource they name is deleted, so the rule does not outlive it.
func (r *Repository) Revoke(ctx context.Context, organizationID, userID string, rules []string) error {
	return r.changeRules(ctx, organizationID, userID, rules, false)
}

// changeRules adds or removes rules on the hidden per-user role atomically.
//
// The script refuses to touch a role that is not hidden: a visible role under
// a user's id means something else claimed the ownership slot, and appending
// there would hand the resource to that role's members. The service reserves
// user-id names, so this is a backstop rather than a reachable path.
func (r *Repository) changeRules(ctx context.Context, organizationID, userID string, rules []string, add bool) error {
	if len(rules) == 0 {
		return nil
	}
	now := time.Now().UTC()
	source := `
		if (ctx._source.hidden != true) {
			throw new IllegalArgumentException("role is not an ownership record");
		}
		if (ctx._source.rules == null) { ctx._source.rules = []; }
		for (rule in params.rules) {
			if (params.add) {
				if (!ctx._source.rules.contains(rule)) { ctx._source.rules.add(rule); }
			} else {
				ctx._source.rules.removeIf(r -> r == rule);
			}
		}
		ctx._source.updated_at = params.now;`

	upsert := Role{
		OrganizationID: organizationID,
		Name:           userID,
		Members:        []string{userID},
		Hidden:         true,
		Description:    "resources this user owns",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// Removing from a role that does not exist yet must not create one
	// carrying the rules it was asked to remove.
	if add {
		upsert.Rules = append([]string(nil), rules...)
	}
	body, err := json.Marshal(map[string]any{
		"script": map[string]any{
			"source": source,
			"lang":   "painless",
			"params": map[string]any{"rules": rules, "add": add, "now": now},
		},
		"upsert": upsert,
	})
	if err != nil {
		return fmt.Errorf("rbac: marshal rules update: %w", err)
	}
	if err := r.os.UpdateDoc(ctx, r.roles, RoleDocID(organizationID, userID), body); err != nil {
		return fmt.Errorf("rbac: change rules for %q: %w", userID, err)
	}
	return nil
}
