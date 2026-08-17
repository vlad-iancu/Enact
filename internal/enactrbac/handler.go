package enactrbac

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// RBACAPI serves organizations, memberships and roles.
//
// Two different gates guard this surface, and the difference matters:
//
//   - **Owner actions** (members, roles, rules) are enforced HERE, by checking
//     the caller's own membership. The service does not trust anyone to have
//     checked before calling.
//   - **Platform-administrator actions** (approving an organization request)
//     are enforced by enact-main, which is the component that knows who the
//     administrator is — ADMIN_EMAIL is an email, and this service only ever
//     sees user ids. The S2S ACL is what keeps those routes service-only.
type RBACAPI struct {
	repo   *rbac.Repository
	logger *logging.Logger
}

type errorResponse struct {
	Error string `json:"error"`
}

type organizationsResponse struct {
	Organizations []rbac.Organization `json:"organizations"`
}

type requestsResponse struct {
	Requests []rbac.OrganizationRequest `json:"requests"`
}

type membersResponse struct {
	Members []rbac.Membership `json:"members"`
}

type rolesResponse struct {
	Roles []rbac.Role `json:"roles"`
}

type createRequestBody struct {
	Name          string `json:"name"`
	Justification string `json:"justification,omitempty"`
	// RequestedByEmail is informational, for the administrator deciding.
	RequestedByEmail string `json:"requested_by_email,omitempty"`
}

type decisionBody struct {
	Reason string `json:"reason,omitempty"`
}

type addMemberBody struct {
	UserID string `json:"user_id"`
	Owner  bool   `json:"owner"`
}

type roleBody struct {
	Name        string   `json:"name"`
	Rules       []string `json:"rules"`
	Description string   `json:"description,omitempty"`
	Members     []string `json:"members,omitempty"`
}

type assignBody struct {
	UserID string `json:"user_id"`
}

type grantBody struct {
	UserID string `json:"user_id"`
	// Rules, when given, are recorded verbatim instead of the ownership
	// rules derived from Resource/ResourceID. It exists for callers that
	// need a PRECISE grant rather than "this user owns this resource" — the
	// e2e suite provisioning its own create permissions, for instance, which
	// must not be expressed as ownership of every resource.
	//
	// Service-to-service only, like the rest of this endpoint: enact-main
	// relays no route to it, so no user can reach it.
	Rules []string `json:"rules,omitempty"`
	// OrganizationID is optional: when empty it is derived from the user's
	// membership, which is the only correct answer anyway.
	OrganizationID string `json:"organization_id,omitempty"`
	Resource       string `json:"resource"`
	ResourceID     string `json:"resource_id"`
}

// WebServices returns the RBAC route groups.
func (a *RBACAPI) WebServices() []*restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1").Produces(restful.MIME_JSON)

	ws.Route(ws.GET("/effective").
		To(a.effective).
		Param(ws.QueryParameter("user_id", "the user to resolve; defaults to the caller").DataType("string")).
		Doc("Resolve a user's organization and every rule they hold — the call services cache and evaluate locally").
		Returns(http.StatusOK, "OK", rbac.Effective{}))

	ws.Route(ws.GET("/roles/mine").
		To(a.myRoles).
		Param(ws.QueryParameter("user_id", "the user to resolve; defaults to the caller").DataType("string")).
		Doc("The caller's own roles in full — needs no ownership, unlike listing an organization's roles").
		Returns(http.StatusOK, "OK", rolesResponse{}))

	ws.Route(ws.POST("/authorize").
		To(a.authorize).
		Consumes(restful.MIME_JSON).
		Doc("Single-shot check: may this user perform this permission?").
		Returns(http.StatusOK, "Decision", authorizeResponse{}))

	// Organization requests. Submitting is any user's right; deciding is the
	// platform administrator's, enforced by enact-main.
	ws.Route(ws.POST("/organizations/requests").
		To(a.createRequest).
		Consumes(restful.MIME_JSON).
		Reads(createRequestBody{}).
		Doc("Request that an organization be created; an administrator must approve it").
		Returns(http.StatusCreated, "Submitted", rbac.OrganizationRequest{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "The caller already belongs to an organization", errorResponse{}))

	ws.Route(ws.GET("/organizations/requests").
		To(a.listRequests).
		Param(ws.QueryParameter("status", "pending|approved|rejected")).
		Param(ws.QueryParameter("requested_by", "filter to one requester")).
		Doc("List organization requests (administrator surface; enact-main gates it)").
		Returns(http.StatusOK, "OK", requestsResponse{}))

	for _, decision := range []struct {
		path    string
		approve bool
	}{{"approve", true}, {"reject", false}} {
		ws.Route(ws.POST("/organizations/requests/{id}/"+decision.path).
			To(a.decideRequestFunc(decision.approve)).
			Consumes(restful.MIME_JSON).
			Param(ws.PathParameter("id", "request id")).
			Reads(decisionBody{}).
			Doc("Decide an organization request; approval creates the organization and makes the requester its first owner").
			Returns(http.StatusOK, "Decided", rbac.OrganizationRequest{}).
			Returns(http.StatusNotFound, "No such request", errorResponse{}).
			Returns(http.StatusConflict, "Already decided", errorResponse{}))
	}

	// Organizations.
	ws.Route(ws.GET("/organizations").
		To(a.listOrganizations).
		Doc("List every organization (administrator surface)").
		Returns(http.StatusOK, "OK", organizationsResponse{}))

	ws.Route(ws.GET("/organizations/{id}").
		To(a.getOrganization).
		Param(ws.PathParameter("id", "organization id")).
		Doc("Get one organization").
		Returns(http.StatusOK, "OK", rbac.Organization{}).
		Returns(http.StatusNotFound, "No such organization", errorResponse{}))

	// Members — owner-gated.
	ws.Route(ws.GET("/organizations/{id}/members").
		To(a.listMembers).
		Param(ws.PathParameter("id", "organization id")).
		Doc("List an organization's members").
		Returns(http.StatusOK, "OK", membersResponse{}).
		Returns(http.StatusForbidden, "Not a member", errorResponse{}))

	ws.Route(ws.POST("/organizations/{id}/members").
		To(a.addMember).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "organization id")).
		Reads(addMemberBody{}).
		Doc("Add a member, or change whether an existing member is an owner. A user already in ANOTHER organization is refused").
		Returns(http.StatusNoContent, "Added", nil).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}).
		Returns(http.StatusConflict, "The user belongs to another organization", errorResponse{}))

	ws.Route(ws.DELETE("/organizations/{id}/members/{user}").
		To(a.removeMember).
		Param(ws.PathParameter("id", "organization id")).
		Param(ws.PathParameter("user", "user id")).
		Doc("Remove a member; the last owner cannot be removed").
		Returns(http.StatusNoContent, "Removed", nil).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}).
		Returns(http.StatusConflict, "The last owner cannot be removed", errorResponse{}))

	// Roles — owner-gated.
	ws.Route(ws.GET("/organizations/{id}/roles").
		To(a.listRoles).
		Param(ws.PathParameter("id", "organization id")).
		Doc("List an organization's roles; the hidden per-user ownership roles are excluded").
		Returns(http.StatusOK, "OK", rolesResponse{}).
		Returns(http.StatusForbidden, "Not a member", errorResponse{}))

	ws.Route(ws.POST("/organizations/{id}/roles").
		To(a.saveRole).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "organization id")).
		Reads(roleBody{}).
		Doc("Create or replace a role and its rules").
		Returns(http.StatusOK, "Saved", rbac.Role{}).
		Returns(http.StatusBadRequest, "Invalid name or rule", errorResponse{}).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}))

	ws.Route(ws.DELETE("/organizations/{id}/roles/{name}").
		To(a.deleteRole).
		Param(ws.PathParameter("id", "organization id")).
		Param(ws.PathParameter("name", "role name")).
		Doc("Delete a role").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}).
		Returns(http.StatusNotFound, "No such role", errorResponse{}))

	for _, action := range []struct {
		path   string
		assign bool
	}{{"assign", true}, {"unassign", false}} {
		ws.Route(ws.POST("/organizations/{id}/roles/{name}/"+action.path).
			To(a.assignRoleFunc(action.assign)).
			Consumes(restful.MIME_JSON).
			Param(ws.PathParameter("id", "organization id")).
			Param(ws.PathParameter("name", "role name")).
			Reads(assignBody{}).
			Doc("Add or remove a member of a role").
			Returns(http.StatusNoContent, "Done", nil).
			Returns(http.StatusForbidden, "Not an owner", errorResponse{}).
			Returns(http.StatusNotFound, "No such role", errorResponse{}))
	}

	// Grants — how a service records that the user who created a resource
	// owns it.
	ws.Route(ws.POST("/memberships").
		To(a.provisionMembership).
		Consumes(restful.MIME_JSON).
		Reads(addMemberBody{}).
		Doc("Place a user in an organization without an owner check (service-only provisioning; enact-main relays no route to it)").
		Returns(http.StatusNoContent, "Placed", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "The user belongs to another organization", errorResponse{}))

	ws.Route(ws.POST("/grants").
		To(a.grant).
		Consumes(restful.MIME_JSON).
		Reads(grantBody{}).
		Doc("Grant a user ownership of a resource they created").
		Returns(http.StatusNoContent, "Granted", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.POST("/grants/revoke").
		To(a.revokeGrant).
		Consumes(restful.MIME_JSON).
		Reads(grantBody{}).
		Doc("Drop the ownership rules for a resource that no longer exists").
		Returns(http.StatusNoContent, "Revoked", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	return []*restful.WebService{ws}
}

type authorizeResponse struct {
	Allowed        bool   `json:"allowed"`
	OrganizationID string `json:"organization_id,omitempty"`
}

// caller is the user this request acts as.
func caller(req *restful.Request) string {
	return identity.FromContext(req.Request.Context())
}

// requireOwner resolves the caller's membership and insists they own the
// named organization. It is the gate on every management route: this service
// does not trust a caller to have been checked upstream.
func (a *RBACAPI) requireOwner(req *restful.Request, resp *restful.Response, organizationID string) bool {
	m, ok := a.requireMember(req, resp, organizationID)
	if !ok {
		return false
	}
	if !m.Owner {
		requesthelper.WriteError(req, resp, http.StatusForbidden,
			"only an organization owner may do this")
		return false
	}
	return true
}

// requireMember insists the caller belongs to the named organization.
func (a *RBACAPI) requireMember(req *restful.Request, resp *restful.Response, organizationID string) (rbac.Membership, bool) {
	userID := caller(req)
	m, found, err := a.repo.GetMembership(req.Request.Context(), userID)
	if err != nil {
		a.logger.Error("failed to load membership", "user_id", userID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to resolve the caller")
		return rbac.Membership{}, false
	}
	// A foreign organization reads as forbidden rather than not-found: the
	// caller named it, so its existence is not the secret — their lack of
	// access is the answer.
	if !found || m.OrganizationID != organizationID {
		requesthelper.WriteError(req, resp, http.StatusForbidden,
			"you do not belong to this organization")
		return rbac.Membership{}, false
	}
	return m, true
}

func (a *RBACAPI) effective(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := req.QueryParameter("user_id")
	if userID == "" {
		userID = caller(req)
	}
	out, err := a.repo.Effective(req.Request.Context(), userID)
	if err != nil {
		logger.Error("failed to resolve effective rules", "user_id", userID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to resolve permissions")
		return
	}
	logger.Info("effective rules resolved", "user_id", userID,
		"organization_id", out.OrganizationID, "owner", out.Owner, "rules", len(out.Rules))
	requesthelper.WriteJSON(req, resp, http.StatusOK, out)
}

// myRoles answers "which roles do I hold". It is deliberately not
// owner-gated: a user seeing their own grants reveals nothing they cannot
// already infer by trying, and hiding it only makes a refusal harder to
// understand.
func (a *RBACAPI) myRoles(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := req.QueryParameter("user_id")
	if userID == "" {
		userID = caller(req)
	}
	roles, err := a.repo.MyRoles(req.Request.Context(), userID)
	if err != nil {
		logger.Error("failed to list the caller's roles", "user_id", userID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list your roles")
		return
	}
	logger.Info("caller roles resolved", "user_id", userID, "count", len(roles))
	requesthelper.WriteJSON(req, resp, http.StatusOK, rolesResponse{Roles: roles})
}

func (a *RBACAPI) authorize(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	var body struct {
		UserID     string `json:"user_id"`
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(req.Request.Body).Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.UserID == "" {
		body.UserID = caller(req)
	}
	out, err := a.repo.Effective(req.Request.Context(), body.UserID)
	if err != nil {
		logger.Error("failed to resolve effective rules", "user_id", body.UserID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to resolve permissions")
		return
	}
	allowed := out.Allows(body.Permission)
	logger.Info("authorization decided", "user_id", body.UserID,
		"permission", body.Permission, "allowed", allowed, "organization_id", out.OrganizationID)
	requesthelper.WriteJSON(req, resp, http.StatusOK,
		authorizeResponse{Allowed: allowed, OrganizationID: out.OrganizationID})
}

func (a *RBACAPI) createRequest(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := caller(req)

	var body createRequestBody
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("organization request submitted", "user_id", userID, "name", body.Name)

	if err := rbac.ValidateName(body.Name); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	// One organization per user: someone who already belongs to one has
	// nothing to request.
	if _, found, err := a.repo.GetMembership(req.Request.Context(), userID); err != nil {
		logger.Error("failed to check membership", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to submit the request")
		return
	} else if found {
		requesthelper.WriteError(req, resp, http.StatusConflict,
			"you already belong to an organization")
		return
	}

	request := rbac.OrganizationRequest{
		ID:               uuid.NewString(),
		Name:             body.Name,
		RequestedBy:      userID,
		RequestedByEmail: body.RequestedByEmail,
		Justification:    body.Justification,
		Status:           rbac.StatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	if err := a.repo.SaveRequest(req.Request.Context(), request); err != nil {
		logger.Error("failed to store the request", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to submit the request")
		return
	}
	logger.Info("organization request stored", "id", request.ID)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, request)
}

func (a *RBACAPI) listRequests(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	status := req.QueryParameter("status")
	requestedBy := req.QueryParameter("requested_by")
	out, err := a.repo.ListRequests(req.Request.Context(), status, requestedBy)
	if err != nil {
		logger.Error("failed to list requests", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list requests")
		return
	}
	logger.Info("organization requests listed", "count", len(out), "status", status)
	requesthelper.WriteJSON(req, resp, http.StatusOK, requestsResponse{Requests: out})
}

// decideRequestFunc returns the handler for one decision. The verb is bound
// at registration rather than sniffed from the path, so the route and the
// behaviour cannot drift apart.
func (a *RBACAPI) decideRequestFunc(approve bool) restful.RouteFunction {
	return func(req *restful.Request, resp *restful.Response) {
		a.decideRequest(req, resp, approve)
	}
}

func (a *RBACAPI) decideRequest(req *restful.Request, resp *restful.Response, approve bool) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")

	var body decisionBody
	_ = json.NewDecoder(req.Request.Body).Decode(&body)
	logger.Info("organization request decision", "id", id, "approve", approve, "decided_by", caller(req))

	ctx := req.Request.Context()
	request, found, err := a.repo.GetRequest(ctx, id)
	if err != nil {
		logger.Error("failed to load the request", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to decide the request")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("request %q not found", id))
		return
	}
	if request.Status != rbac.StatusPending {
		requesthelper.WriteError(req, resp, http.StatusConflict,
			fmt.Sprintf("this request was already %s", request.Status))
		return
	}

	now := time.Now().UTC()
	request.DecidedBy = caller(req)
	request.DecidedAt = &now
	request.Reason = body.Reason

	if !approve {
		request.Status = rbac.StatusRejected
		if err := a.repo.SaveRequest(ctx, request); err != nil {
			logger.Error("failed to store the decision", "err", err)
			requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to decide the request")
			return
		}
		logger.Info("organization request rejected", "id", id)
		requesthelper.WriteJSON(req, resp, http.StatusOK, request)
		return
	}

	// Approval creates the organization and makes the requester its first
	// owner. Order matters: the organization must exist before anyone is
	// placed in it, and the request is marked approved last, so a failure
	// leaves a pending request to retry rather than an orphan organization.
	org := rbac.Organization{
		ID:        uuid.NewString(),
		Name:      request.Name,
		CreatedBy: request.RequestedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.repo.SaveOrganization(ctx, org); err != nil {
		logger.Error("failed to create the organization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the organization")
		return
	}
	membership := rbac.Membership{
		UserID:         request.RequestedBy,
		OrganizationID: org.ID,
		Owner:          true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.repo.SaveMembership(ctx, membership); err != nil {
		logger.Error("failed to place the requester in the organization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the organization")
		return
	}
	request.Status = rbac.StatusApproved
	request.OrganizationID = org.ID
	if err := a.repo.SaveRequest(ctx, request); err != nil {
		logger.Error("failed to store the decision", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to decide the request")
		return
	}
	logger.Info("organization created", "id", org.ID, "name", org.Name, "owner", request.RequestedBy)
	requesthelper.WriteJSON(req, resp, http.StatusOK, request)
}

func (a *RBACAPI) listOrganizations(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	out, err := a.repo.ListOrganizations(req.Request.Context())
	if err != nil {
		logger.Error("failed to list organizations", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list organizations")
		return
	}
	logger.Info("organizations listed", "count", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, organizationsResponse{Organizations: out})
}

func (a *RBACAPI) getOrganization(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	org, found, err := a.repo.GetOrganization(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to load the organization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to load the organization")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("organization %q not found", id))
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, org)
}

func (a *RBACAPI) listMembers(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	if _, ok := a.requireMember(req, resp, id); !ok {
		return
	}
	out, err := a.repo.ListMembers(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to list members", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list members")
		return
	}
	logger.Info("members listed", "organization_id", id, "count", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, membersResponse{Members: out})
}

func (a *RBACAPI) addMember(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	if !a.requireOwner(req, resp, id) {
		return
	}
	var body addMemberBody
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.UserID == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "user_id is required")
		return
	}
	ctx := req.Request.Context()
	existing, found, err := a.repo.GetMembership(ctx, body.UserID)
	if err != nil {
		logger.Error("failed to check the user's membership", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to add the member")
		return
	}
	// A membership never changes organization. A resource's organization is
	// inferred from its owner, so moving a user would drag everything they
	// ever created across the boundary with them.
	if found && existing.OrganizationID != id {
		logger.Warn("refused to move a user between organizations",
			"user_id", body.UserID, "from", existing.OrganizationID, "to", id)
		requesthelper.WriteError(req, resp, http.StatusConflict,
			"this user belongs to another organization; remove them from it first")
		return
	}

	now := time.Now().UTC()
	membership := rbac.Membership{
		UserID:         body.UserID,
		OrganizationID: id,
		Owner:          body.Owner,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if found {
		membership.CreatedAt = existing.CreatedAt
	}
	if err := a.repo.SaveMembership(ctx, membership); err != nil {
		logger.Error("failed to store the membership", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to add the member")
		return
	}
	logger.Info("member added", "organization_id", id, "user_id", body.UserID, "owner", body.Owner)
	resp.WriteHeader(http.StatusNoContent)
}

func (a *RBACAPI) removeMember(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	userID := req.PathParameter("user")
	if !a.requireOwner(req, resp, id) {
		return
	}
	ctx := req.Request.Context()
	members, err := a.repo.ListMembers(ctx, id)
	if err != nil {
		logger.Error("failed to list members", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to remove the member")
		return
	}
	owners, isMember := 0, false
	for _, m := range members {
		if m.Owner {
			owners++
		}
		if m.UserID == userID {
			isMember = true
		}
	}
	if !isMember {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "this user is not a member")
		return
	}
	// An organization without an owner cannot be administered by anyone.
	for _, m := range members {
		if m.UserID == userID && m.Owner && owners == 1 {
			requesthelper.WriteError(req, resp, http.StatusConflict,
				"this is the last owner; promote another owner first")
			return
		}
	}
	if err := a.repo.DeleteMembership(ctx, userID); err != nil {
		logger.Error("failed to remove the membership", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to remove the member")
		return
	}
	logger.Info("member removed", "organization_id", id, "user_id", userID)
	resp.WriteHeader(http.StatusNoContent)
}

func (a *RBACAPI) listRoles(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	if _, ok := a.requireMember(req, resp, id); !ok {
		return
	}
	out, err := a.repo.ListRoles(req.Request.Context(), id, false)
	if err != nil {
		logger.Error("failed to list roles", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list roles")
		return
	}
	logger.Info("roles listed", "organization_id", id, "count", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, rolesResponse{Roles: out})
}

func (a *RBACAPI) saveRole(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	if !a.requireOwner(req, resp, id) {
		return
	}
	var body roleBody
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("role save requested", "organization_id", id, "name", body.Name,
		"rules", len(body.Rules), "members", len(body.Members))

	if err := rbac.ValidateName(body.Name); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	if err := rbac.ValidateRules(body.Rules); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx := req.Request.Context()
	existing, found, err := a.repo.GetRole(ctx, id, body.Name)
	if err != nil {
		logger.Error("failed to load the role", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to save the role")
		return
	}
	// The hidden ownership roles are bookkeeping: an owner editing one by
	// name would be rewriting who owns what behind the platform's back.
	if found && existing.Hidden {
		requesthelper.WriteError(req, resp, http.StatusForbidden,
			"this name belongs to a user's ownership record and cannot be edited as a role")
		return
	}
	// The same reservation, one step earlier. Ownership is stored in a role
	// NAMED after the user, so a visible role created under a user id before
	// that user owns anything would become the record their ownership rules
	// are appended to — handing every member of that role whatever the user
	// later creates. Refused whether or not the ownership role exists yet,
	// which is what makes the guard above complete.
	if _, isUser, err := a.repo.GetMembership(ctx, body.Name); err != nil {
		logger.Error("failed to check the role name against memberships", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to save the role")
		return
	} else if isUser {
		logger.Warn("role name collides with a user id", "name", body.Name)
		requesthelper.WriteError(req, resp, http.StatusConflict,
			"this name is a user id, which is reserved for that user's ownership record; choose another name")
		return
	}
	// Members must belong to this organization, or a role would grant rules
	// to somebody outside it.
	for _, member := range body.Members {
		m, found, err := a.repo.GetMembership(ctx, member)
		if err != nil {
			logger.Error("failed to check a member", "user_id", member, "err", err)
			requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to save the role")
			return
		}
		if !found || m.OrganizationID != id {
			requesthelper.WriteError(req, resp, http.StatusBadRequest,
				fmt.Sprintf("user %q does not belong to this organization", member))
			return
		}
	}

	now := time.Now().UTC()
	role := rbac.Role{
		OrganizationID: id,
		Name:           body.Name,
		Rules:          body.Rules,
		Members:        body.Members,
		Description:    body.Description,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if found {
		role.CreatedAt = existing.CreatedAt
		if body.Members == nil {
			role.Members = existing.Members
		}
	}
	if err := a.repo.SaveRole(ctx, role); err != nil {
		logger.Error("failed to store the role", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to save the role")
		return
	}
	logger.Info("role saved", "organization_id", id, "name", role.Name, "rules", len(role.Rules))
	requesthelper.WriteJSON(req, resp, http.StatusOK, role)
}

func (a *RBACAPI) deleteRole(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	name := req.PathParameter("name")
	if !a.requireOwner(req, resp, id) {
		return
	}
	ctx := req.Request.Context()
	role, found, err := a.repo.GetRole(ctx, id, name)
	if err != nil {
		logger.Error("failed to load the role", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete the role")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("role %q not found", name))
		return
	}
	if role.Hidden {
		requesthelper.WriteError(req, resp, http.StatusForbidden,
			"this name belongs to a user's ownership record and cannot be deleted as a role")
		return
	}
	if err := a.repo.DeleteRole(ctx, id, name); err != nil {
		logger.Error("failed to delete the role", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete the role")
		return
	}
	logger.Info("role deleted", "organization_id", id, "name", name)
	resp.WriteHeader(http.StatusNoContent)
}

// assignRoleFunc returns the handler for assignment or its reverse.
func (a *RBACAPI) assignRoleFunc(assign bool) restful.RouteFunction {
	return func(req *restful.Request, resp *restful.Response) {
		a.assignRole(req, resp, assign)
	}
}

func (a *RBACAPI) assignRole(req *restful.Request, resp *restful.Response, assign bool) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.PathParameter("id")
	name := req.PathParameter("name")
	if !a.requireOwner(req, resp, id) {
		return
	}
	var body assignBody
	if err := json.NewDecoder(req.Request.Body).Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.UserID == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "user_id is required")
		return
	}
	ctx := req.Request.Context()
	role, found, err := a.repo.GetRole(ctx, id, name)
	if err != nil {
		logger.Error("failed to load the role", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to change the assignment")
		return
	}
	if !found || role.Hidden {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("role %q not found", name))
		return
	}
	if assign {
		m, memberFound, err := a.repo.GetMembership(ctx, body.UserID)
		if err != nil {
			logger.Error("failed to check the member", "err", err)
			requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to change the assignment")
			return
		}
		if !memberFound || m.OrganizationID != id {
			requesthelper.WriteError(req, resp, http.StatusBadRequest,
				fmt.Sprintf("user %q does not belong to this organization", body.UserID))
			return
		}
	}
	role.Members = withMember(role.Members, body.UserID, assign)
	role.UpdatedAt = time.Now().UTC()
	if err := a.repo.SaveRole(ctx, role); err != nil {
		logger.Error("failed to store the role", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to change the assignment")
		return
	}
	logger.Info("role assignment changed", "organization_id", id, "name", name,
		"user_id", body.UserID, "assigned", assign, "members", len(role.Members))
	resp.WriteHeader(http.StatusNoContent)
}

func (a *RBACAPI) grant(req *restful.Request, resp *restful.Response) {
	a.changeGrant(req, resp, true)
}

func (a *RBACAPI) revokeGrant(req *restful.Request, resp *restful.Response) {
	a.changeGrant(req, resp, false)
}

// changeGrant adds or removes a user's ownership of one resource. The
// organization is derived from the user's membership rather than trusted from
// the body: the caller is a service reporting what it just created, and the
// only organization that can be correct is the one the user is in.
func (a *RBACAPI) changeGrant(req *restful.Request, resp *restful.Response, add bool) {
	logger := requesthelper.Logger(req, a.logger)
	var body grantBody
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.UserID == "" {
		body.UserID = caller(req)
	}
	if len(body.Rules) == 0 && (body.Resource == "" || body.ResourceID == "") {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "resource and resource_id are required unless rules are given")
		return
	}
	if err := rbac.ValidateRules(body.Rules); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	ctx := req.Request.Context()
	m, found, err := a.repo.GetMembership(ctx, body.UserID)
	if err != nil {
		logger.Error("failed to resolve the user's organization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to record the grant")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusBadRequest,
			fmt.Sprintf("user %q belongs to no organization, so they cannot own anything", body.UserID))
		return
	}

	rules := body.Rules
	if len(rules) == 0 {
		rules = rbac.OwnerRules(body.Resource, body.ResourceID)
	}
	if add {
		err = a.repo.Grant(ctx, m.OrganizationID, body.UserID, rules)
	} else {
		err = a.repo.Revoke(ctx, m.OrganizationID, body.UserID, rules)
	}
	if err != nil {
		logger.Error("failed to change the grant", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to record the grant")
		return
	}
	logger.Info("rules recorded", "user_id", body.UserID, "organization_id", m.OrganizationID,
		"resource", body.Resource, "resource_id", body.ResourceID, "rules", len(rules), "granted", add)
	resp.WriteHeader(http.StatusNoContent)
}

// provisionMembership places a user in an organization WITHOUT requiring the
// caller to be an owner. It is the membership counterpart of /v1/grants: a
// service stating a fact it is entitled to state, rather than a person
// exercising a permission.
//
// Why it exists: an organization's first owner is created by approving a
// request, and every member after that is added by an owner. Neither route is
// available to a provisioning caller that is not a person — the e2e suite
// placing its own fixture accounts, or a future bootstrap tool. Without this
// they would have to impersonate a real owner.
//
// Kept safe by two things: the S2S ACL, which admits only services, and the
// same immutability guard as addMember — a user is never MOVED between
// organizations, because a resource's organization is inferred from its
// owner and moving the owner would drag everything they made across the
// boundary.
func (a *RBACAPI) provisionMembership(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	var body struct {
		UserID         string `json:"user_id"`
		OrganizationID string `json:"organization_id"`
		Owner          bool   `json:"owner"`
	}
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.UserID == "" || body.OrganizationID == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "user_id and organization_id are required")
		return
	}
	ctx := req.Request.Context()
	if _, found, err := a.repo.GetOrganization(ctx, body.OrganizationID); err != nil {
		logger.Error("failed to read the organization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to place the member")
		return
	} else if !found {
		requesthelper.WriteError(req, resp, http.StatusBadRequest,
			fmt.Sprintf("organization %q does not exist", body.OrganizationID))
		return
	}
	existing, found, err := a.repo.GetMembership(ctx, body.UserID)
	if err != nil {
		logger.Error("failed to check the user's membership", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to place the member")
		return
	}
	if found && existing.OrganizationID != body.OrganizationID {
		requesthelper.WriteError(req, resp, http.StatusConflict,
			"this user belongs to another organization; remove them from it first")
		return
	}
	if found && existing.Owner == body.Owner {
		resp.WriteHeader(http.StatusNoContent)
		return
	}
	now := time.Now().UTC()
	membership := rbac.Membership{
		UserID:         body.UserID,
		OrganizationID: body.OrganizationID,
		Owner:          body.Owner,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if found {
		membership.CreatedAt = existing.CreatedAt
	}
	if err := a.repo.SaveMembership(ctx, membership); err != nil {
		logger.Error("failed to store the membership", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to place the member")
		return
	}
	logger.Info("membership provisioned", "user_id", body.UserID,
		"organization_id", body.OrganizationID, "owner", body.Owner)
	resp.WriteHeader(http.StatusNoContent)
}

// withMember adds or removes one user from a role's member list.
func withMember(members []string, userID string, add bool) []string {
	out := make([]string, 0, len(members)+1)
	for _, m := range members {
		if m != userID {
			out = append(out, m)
		}
	}
	if add {
		out = append(out, userID)
	}
	return out
}
