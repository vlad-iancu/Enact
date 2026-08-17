package rbac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enact/internal/identity"
	"enact/internal/requesthelper"
)

// ClientConfig points a caller at the RBAC service.
type ClientConfig struct {
	BaseURL string        `env:"RBAC_API_URL, default=http://localhost:8009"`
	Timeout time.Duration `env:"RBAC_API_TIMEOUT, default=10s"`
	// EffectiveTTL is how long a user's resolved organization and rules are
	// cached in-process by an Enforcer. It trades staleness for round trips:
	// a role revoked at the service stays live for at most this long.
	EffectiveTTL time.Duration `env:"RBAC_EFFECTIVE_TTL, default=10s"`
}

// Client talks to enact-rbac. It lives in the domain package so callers
// depend on the domain, never on another service's internals (ADR-0004).
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient returns a Client for the service at cfg.BaseURL. base is the
// caller's S2S signing transport.
func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	return &Client{
		http:    &http.Client{Transport: requesthelper.NewTransport(base), Timeout: cfg.Timeout},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
	}
}

// ForbiddenError is a 403 from the RBAC service: the caller is known, and is
// not permitted. Distinct from a transport failure, because only one of the
// two is the caller's to fix.
type ForbiddenError struct{ Message string }

func (e *ForbiddenError) Error() string { return e.Message }

// do issues one JSON request. 404 maps to found=false; 403 to
// *ForbiddenError; 400/409 to *requesthelper.BadRequestError.
func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, wantStatus int, out any) (bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, fmt.Errorf("rbac: build %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("rbac: %s %s: %w", method, endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case wantStatus:
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return false, fmt.Errorf("rbac: decode response: %w", err)
			}
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusForbidden, http.StatusBadRequest, http.StatusConflict:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = http.StatusText(resp.StatusCode)
		}
		if resp.StatusCode == http.StatusForbidden {
			return false, &ForbiddenError{Message: apiErr.Error}
		}
		return false, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return false, fmt.Errorf("rbac: %s %s: unexpected status %d", method, endpoint, resp.StatusCode)
	}
}

// Effective resolves one user's organization and rules — the call every
// service makes to decide anything. Prefer an Enforcer, which caches it.
func (c *Client) Effective(ctx context.Context, userID string) (Effective, error) {
	var out Effective
	endpoint := c.baseURL + "/v1/effective?user_id=" + url.QueryEscape(userID)
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return Effective{}, err
	}
	return out, nil
}

// GrantRequest makes a user the owner of a resource, or — when Rules is set —
// gives them exactly those rules instead. Ownership is the common case;
// explicit rules exist for callers that need a narrower grant than "owns this
// resource", such as a test fixture provisioning create permissions.
type GrantRequest struct {
	UserID         string   `json:"user_id"`
	OrganizationID string   `json:"organization_id"`
	Resource       string   `json:"resource,omitempty"`
	ResourceID     string   `json:"resource_id,omitempty"`
	Rules          []string `json:"rules,omitempty"`
}

// Grant records that a user owns a resource they just created. Called by
// every service that creates one.
func (c *Client) Grant(ctx context.Context, body GrantRequest) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("rbac: marshal grant: %w", err)
	}
	_, err = c.do(ctx, http.MethodPost, c.baseURL+"/v1/grants", payload, http.StatusNoContent, nil)
	return err
}

// Revoke drops the ownership rules for a resource that no longer exists.
func (c *Client) Revoke(ctx context.Context, body GrantRequest) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("rbac: marshal revoke: %w", err)
	}
	_, err = c.do(ctx, http.MethodPost, c.baseURL+"/v1/grants/revoke", payload, http.StatusNoContent, nil)
	return err
}

// --- organizations, requests, roles: the management surface ----------------

// CreateOrganizationRequest is a user asking for an organization.
type CreateOrganizationRequest struct {
	Name          string `json:"name"`
	Justification string `json:"justification,omitempty"`
}

// RequestOrganization submits a request for the calling user.
func (c *Client) RequestOrganization(ctx context.Context, body CreateOrganizationRequest) (OrganizationRequest, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return OrganizationRequest{}, err
	}
	var out OrganizationRequest
	if _, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/organizations/requests", payload, http.StatusCreated, &out); err != nil {
		return OrganizationRequest{}, err
	}
	return out, nil
}

// ListOrganizationRequests returns requests, optionally filtered by status.
func (c *Client) ListOrganizationRequests(ctx context.Context, status string) ([]OrganizationRequest, error) {
	endpoint := c.baseURL + "/v1/organizations/requests"
	if status != "" {
		endpoint += "?status=" + url.QueryEscape(status)
	}
	var out struct {
		Requests []OrganizationRequest `json:"requests"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// ListOrganizationRequestsBy returns one user's own requests — what a person
// sees about their own pending application, as opposed to the
// administrator's queue.
func (c *Client) ListOrganizationRequestsBy(ctx context.Context, userID string) ([]OrganizationRequest, error) {
	endpoint := c.baseURL + "/v1/organizations/requests?requested_by=" + url.QueryEscape(userID)
	var out struct {
		Requests []OrganizationRequest `json:"requests"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// DecideRequest approves or rejects an organization request.
func (c *Client) DecideRequest(ctx context.Context, id string, approve bool, reason string) (OrganizationRequest, bool, error) {
	action := "reject"
	if approve {
		action = "approve"
	}
	payload, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return OrganizationRequest{}, false, err
	}
	var out OrganizationRequest
	found, err := c.do(ctx, http.MethodPost,
		c.baseURL+"/v1/organizations/requests/"+url.PathEscape(id)+"/"+action, payload, http.StatusOK, &out)
	if err != nil {
		return OrganizationRequest{}, false, err
	}
	return out, found, nil
}

// Organization fetches one organization.
func (c *Client) Organization(ctx context.Context, id string) (Organization, bool, error) {
	var out Organization
	found, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/organizations/"+url.PathEscape(id), nil, http.StatusOK, &out)
	if err != nil {
		return Organization{}, false, err
	}
	return out, found, nil
}

// Members lists an organization's members.
func (c *Client) Members(ctx context.Context, organizationID string) ([]Membership, error) {
	var out struct {
		Members []Membership `json:"members"`
	}
	endpoint := c.baseURL + "/v1/organizations/" + url.PathEscape(organizationID) + "/members"
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

// AddMemberRequest places a user in an organization.
type AddMemberRequest struct {
	UserID string `json:"user_id"`
	Owner  bool   `json:"owner"`
}

// AddMember adds (or re-places) a user in an organization.
func (c *Client) AddMember(ctx context.Context, organizationID string, body AddMemberRequest) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := c.baseURL + "/v1/organizations/" + url.PathEscape(organizationID) + "/members"
	_, err = c.do(ctx, http.MethodPost, endpoint, payload, http.StatusNoContent, nil)
	return err
}

// RemoveMember removes a user from an organization.
func (c *Client) RemoveMember(ctx context.Context, organizationID, userID string) (bool, error) {
	endpoint := c.baseURL + "/v1/organizations/" + url.PathEscape(organizationID) + "/members/" + url.PathEscape(userID)
	return c.do(ctx, http.MethodDelete, endpoint, nil, http.StatusNoContent, nil)
}

// RoleRequest creates or replaces a role.
type RoleRequest struct {
	Name        string   `json:"name"`
	Rules       []string `json:"rules"`
	Description string   `json:"description,omitempty"`
	Members     []string `json:"members,omitempty"`
}

// SaveRole creates or replaces a role in an organization.
func (c *Client) SaveRole(ctx context.Context, organizationID string, body RoleRequest) (Role, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Role{}, err
	}
	var out Role
	endpoint := c.baseURL + "/v1/organizations/" + url.PathEscape(organizationID) + "/roles"
	if _, err := c.do(ctx, http.MethodPost, endpoint, payload, http.StatusOK, &out); err != nil {
		return Role{}, err
	}
	return out, nil
}

// MyRoles returns one user's own roles in full. Caller-scoped: it needs no
// ownership, which is what lets a profile page show them.
func (c *Client) MyRoles(ctx context.Context, userID string) ([]Role, error) {
	var out struct {
		Roles []Role `json:"roles"`
	}
	endpoint := c.baseURL + "/v1/roles/mine?user_id=" + url.QueryEscape(userID)
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Roles, nil
}

// ListRoles returns an organization's visible roles.
func (c *Client) ListRoles(ctx context.Context, organizationID string) ([]Role, error) {
	var out struct {
		Roles []Role `json:"roles"`
	}
	endpoint := c.baseURL + "/v1/organizations/" + url.PathEscape(organizationID) + "/roles"
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Roles, nil
}

// DeleteRole removes a role.
func (c *Client) DeleteRole(ctx context.Context, organizationID, name string) (bool, error) {
	endpoint := c.baseURL + "/v1/organizations/" + url.PathEscape(organizationID) + "/roles/" + url.PathEscape(name)
	return c.do(ctx, http.MethodDelete, endpoint, nil, http.StatusNoContent, nil)
}

// AssignRole adds or removes a member of a role.
func (c *Client) AssignRole(ctx context.Context, organizationID, name, userID string, assign bool) (bool, error) {
	action := "unassign"
	if assign {
		action = "assign"
	}
	payload, err := json.Marshal(map[string]string{"user_id": userID})
	if err != nil {
		return false, err
	}
	endpoint := c.baseURL + "/v1/organizations/" + url.PathEscape(organizationID) +
		"/roles/" + url.PathEscape(name) + "/" + action
	return c.do(ctx, http.MethodPost, endpoint, payload, http.StatusNoContent, nil)
}
