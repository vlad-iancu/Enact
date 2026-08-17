package enactmain

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"enact/internal/rbac"
	"enact/internal/requesthelper"
	"enact/internal/users"
)

// organizationsWebService is the browser surface over enact-rbac: requesting
// an organization, and — for its owners — managing its members and roles.
//
// Owner-gating is NOT done here. The RBAC service checks the caller's own
// membership on every management route, so this group only supplies the
// session's identity and relays the answer. Doing it in both places would
// mean two rules to keep in step.
func (a *MainAPI) organizationsWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/organizations").Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireSession)

	ws.Route(ws.GET("/me").
		To(a.myOrganization).
		Doc("The organization the logged-in user belongs to, and whether they own it").
		Returns(http.StatusOK, "OK", myOrganizationResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.GET("/me/roles").
		To(a.myRoles).
		Doc("The roles the logged-in user holds, with what each one grants").
		Returns(http.StatusOK, "OK", myRolesResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.POST("/requests").
		To(a.requestOrganization).
		Consumes(restful.MIME_JSON).
		Reads(rbac.CreateOrganizationRequest{}).
		Doc("Ask for an organization to be created; an administrator must approve it").
		Returns(http.StatusCreated, "Submitted", rbac.OrganizationRequest{}).
		Returns(http.StatusConflict, "Already in an organization", errorResponse{}))

	ws.Route(ws.GET("/requests").
		To(a.myOrganizationRequests).
		Doc("The logged-in user's own organization requests and their status").
		Returns(http.StatusOK, "OK", organizationRequestsResponse{}))

	ws.Route(ws.GET("/{id}/members").
		To(a.listOrganizationMembers).
		Param(ws.PathParameter("id", "organization id")).
		Doc("List the organization's members").
		Returns(http.StatusOK, "OK", organizationMembersResponse{}).
		Returns(http.StatusForbidden, "Not a member", errorResponse{}))

	ws.Route(ws.POST("/{id}/members").
		To(a.addOrganizationMember).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "organization id")).
		Reads(rbac.AddMemberRequest{}).
		Doc("Add an existing user to the organization, or promote a member to owner (owners only)").
		Returns(http.StatusNoContent, "Added", nil).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}))

	ws.Route(ws.DELETE("/{id}/members/{user}").
		To(a.removeOrganizationMember).
		Param(ws.PathParameter("id", "organization id")).
		Param(ws.PathParameter("user", "user id")).
		Doc("Remove a member (owners only)").
		Returns(http.StatusNoContent, "Removed", nil).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}))

	ws.Route(ws.POST("/{id}/users").
		To(a.createOrganizationUser).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "organization id")).
		Reads(createOrganizationUserRequest{}).
		Doc("Create a user inside the organization (owners only). The account is verified from birth, like the administrator's create-user").
		Returns(http.StatusCreated, "Created", userResponse{}).
		Returns(http.StatusConflict, "Email already registered", errorResponse{}).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}))

	ws.Route(ws.GET("/{id}/roles").
		To(a.listOrganizationRoles).
		Param(ws.PathParameter("id", "organization id")).
		Doc("List the organization's roles").
		Returns(http.StatusOK, "OK", organizationRolesResponse{}).
		Returns(http.StatusForbidden, "Not a member", errorResponse{}))

	ws.Route(ws.POST("/{id}/roles").
		To(a.saveOrganizationRole).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "organization id")).
		Reads(rbac.RoleRequest{}).
		Doc("Create or replace a role and the rules it grants (owners only)").
		Returns(http.StatusOK, "Saved", rbac.Role{}).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}))

	ws.Route(ws.DELETE("/{id}/roles/{name}").
		To(a.deleteOrganizationRole).
		Param(ws.PathParameter("id", "organization id")).
		Param(ws.PathParameter("name", "role name")).
		Doc("Delete a role (owners only)").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusForbidden, "Not an owner", errorResponse{}))

	for _, action := range []struct {
		path   string
		assign bool
	}{{"assign", true}, {"unassign", false}} {
		ws.Route(ws.POST("/{id}/roles/{name}/"+action.path).
			To(a.assignOrganizationRoleFunc(action.assign)).
			Consumes(restful.MIME_JSON).
			Param(ws.PathParameter("id", "organization id")).
			Param(ws.PathParameter("name", "role name")).
			Doc("Add or remove a member of a role (owners only)").
			Returns(http.StatusNoContent, "Done", nil).
			Returns(http.StatusForbidden, "Not an owner", errorResponse{}))
	}

	return ws
}

type myOrganizationResponse struct {
	OrganizationID string `json:"organization_id,omitempty"`
	Name           string `json:"name,omitempty"`
	Owner          bool   `json:"owner"`
	// Rules is what the user may do, for a UI that wants to hide what it
	// would only get refused for.
	Rules []string `json:"rules,omitempty"`
	// Roles names the roles those rules came from. Ownership does not appear
	// here — an owner holds every permission without a role saying so, which
	// the Owner flag already reports.
	Roles []string `json:"roles,omitempty"`
}

type myRolesResponse struct {
	Roles []rbac.Role `json:"roles"`
}

type organizationRequestsResponse struct {
	Requests []rbac.OrganizationRequest `json:"requests"`
}

// organizationMember is a membership with the person attached. enact-rbac
// deals only in user ids — it has no access to the users index and should
// not — so the two halves are joined here, the one place that holds both.
type organizationMember struct {
	UserID string `json:"user_id"`
	Owner  bool   `json:"owner"`
	// DisplayName, Email and AvatarURL are absent when no account matches the
	// id: a membership can outlive the account it names, and the listing must
	// still render rather than hide the row.
	DisplayName string    `json:"display_name,omitempty"`
	Email       string    `json:"email,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type organizationMembersResponse struct {
	Members []organizationMember `json:"members"`
}

type organizationRolesResponse struct {
	Roles []rbac.Role `json:"roles"`
}

type createOrganizationUserRequest struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	Owner       bool   `json:"owner,omitempty"`
}

func (a *MainAPI) myOrganization(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	req = identityCtx(req)
	effective, err := a.rbac.Effective(req.Request.Context(), sess.UserID)
	if err != nil {
		logger.Error("failed to resolve the caller's organization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to resolve your organization")
		return
	}
	out := myOrganizationResponse{
		OrganizationID: effective.OrganizationID,
		Owner:          effective.Owner,
		Rules:          effective.Rules,
		Roles:          effective.Roles,
	}
	if effective.OrganizationID != "" {
		if org, found, err := a.rbac.Organization(req.Request.Context(), effective.OrganizationID); err == nil && found {
			out.Name = org.Name
		}
	}
	logger.Info("organization resolved", "organization_id", out.OrganizationID, "owner", out.Owner)
	requesthelper.WriteJSON(req, resp, http.StatusOK, out)
}

// myRoles lists the caller's own roles in full, for a profile page that wants
// to say what each one grants rather than only naming it.
func (a *MainAPI) myRoles(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	req = identityCtx(req)
	roles, err := a.rbac.MyRoles(req.Request.Context(), sess.UserID)
	if err != nil {
		logger.Error("failed to list the caller's roles", "err", err)
		relayRBACErr(req, resp, err, "list your roles")
		return
	}
	logger.Info("caller roles listed", "count", len(roles))
	requesthelper.WriteJSON(req, resp, http.StatusOK, myRolesResponse{Roles: roles})
}

func (a *MainAPI) requestOrganization(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	var body rbac.CreateOrganizationRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("organization requested", "name", body.Name)
	req = identityCtx(req)
	out, err := a.rbac.RequestOrganization(req.Request.Context(), body)
	if err != nil {
		logger.Warn("organization request failed", "err", err)
		relayRBACErr(req, resp, err, "submit the request")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusCreated, out)
}

func (a *MainAPI) myOrganizationRequests(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	req = identityCtx(req)
	// Scoped to the caller: a user sees their own requests, never anyone
	// else's.
	out, err := a.rbac.ListOrganizationRequestsBy(req.Request.Context(), sess.UserID)
	if err != nil {
		logger.Error("failed to list the caller's requests", "err", err)
		relayRBACErr(req, resp, err, "list your requests")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, organizationRequestsResponse{Requests: out})
}

func (a *MainAPI) listOrganizationMembers(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	req = identityCtx(req)
	members, err := a.rbac.Members(req.Request.Context(), req.PathParameter("id"))
	if err != nil {
		logger.Warn("failed to list members", "err", err)
		relayRBACErr(req, resp, err, "list the members")
		return
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	// One lookup for the whole page, not one per member.
	accounts, err := a.users.ByIDs(req.Request.Context(), ids)
	if err != nil {
		// The membership list is the answer; the names are decoration. A
		// failure here degrades the response rather than failing it.
		logger.Warn("failed to resolve member accounts; listing ids only", "err", err)
		accounts = nil
	}
	out := make([]organizationMember, 0, len(members))
	for _, m := range members {
		entry := organizationMember{UserID: m.UserID, Owner: m.Owner, CreatedAt: m.CreatedAt}
		if u, ok := accounts[m.UserID]; ok {
			entry.DisplayName = u.DisplayName
			entry.Email = u.Email
			entry.AvatarURL = a.avatarURL(u.AvatarKey)
		}
		out = append(out, entry)
	}
	logger.Info("members listed", "count", len(out), "resolved", len(accounts))
	requesthelper.WriteJSON(req, resp, http.StatusOK, organizationMembersResponse{Members: out})
}

func (a *MainAPI) addOrganizationMember(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	var body rbac.AddMemberRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	req = identityCtx(req)
	if err := a.rbac.AddMember(req.Request.Context(), req.PathParameter("id"), body); err != nil {
		logger.Warn("failed to add the member", "err", err)
		relayRBACErr(req, resp, err, "add the member")
		return
	}
	logger.Info("member added", "user_id", body.UserID, "owner", body.Owner)
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) removeOrganizationMember(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	req = identityCtx(req)
	found, err := a.rbac.RemoveMember(req.Request.Context(), req.PathParameter("id"), req.PathParameter("user"))
	if err != nil {
		logger.Warn("failed to remove the member", "err", err)
		relayRBACErr(req, resp, err, "remove the member")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "this user is not a member")
		return
	}
	resp.WriteHeader(http.StatusNoContent)
}

// createOrganizationUser is the owner's version of admin create-user: it
// makes an account AND places it in the owner's organization, which is the
// only way a new user can do anything at all.
func (a *MainAPI) createOrganizationUser(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	organizationID := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "organization_id", organizationID)

	var body createOrganizationUserRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	email := users.NormalizeEmail(body.Email)
	logger.Info("organization user creation requested", "email", email, "owner", body.Owner)
	if email == "" || body.Password == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "email and password are required")
		return
	}

	// Authorization first, before touching the users index: the RBAC service
	// refuses a caller who does not own this organization, and creating the
	// account before learning that would leave an orphan.
	ctx := identityCtx(req).Request.Context()
	effective, err := a.rbac.Effective(ctx, sess.UserID)
	if err != nil {
		logger.Error("failed to resolve the caller", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to check permissions")
		return
	}
	if effective.OrganizationID != organizationID || !effective.Owner {
		requesthelper.WriteError(req, resp, http.StatusForbidden, "only an owner of this organization may create users in it")
		return
	}

	if _, found, err := a.users.GetByEmail(ctx, email); err != nil {
		logger.Error("failed to check the email", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the user")
		return
	} else if found {
		requesthelper.WriteError(req, resp, http.StatusConflict, "an account with this email already exists")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		logger.Error("failed to hash the password", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the user")
		return
	}
	now := time.Now().UTC()
	user := users.User{
		ID:          uuid.NewString(),
		Email:       email,
		DisplayName: strings.TrimSpace(body.DisplayName),
		// Verified from birth: an owner vouching for someone is the same
		// assurance the administrator's create-user relies on.
		EmailVerified: true,
		PasswordHash:  string(hash),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := a.users.Save(ctx, user); err != nil {
		logger.Error("failed to store the user", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create the user")
		return
	}
	// Membership second: an account with no organization can do nothing, so
	// a failure here leaves a user the owner can retry placing rather than a
	// member of an organization with no account.
	if err := a.rbac.AddMember(ctx, organizationID, rbac.AddMemberRequest{UserID: user.ID, Owner: body.Owner}); err != nil {
		logger.Error("failed to place the new user in the organization", "err", err)
		relayRBACErr(req, resp, err, "place the user in the organization")
		return
	}
	logger.Info("organization user created", "created_user_id", user.ID, "email", email)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, a.toUserResponse(user))
}

func (a *MainAPI) listOrganizationRoles(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	req = identityCtx(req)
	out, err := a.rbac.ListRoles(req.Request.Context(), req.PathParameter("id"))
	if err != nil {
		logger.Warn("failed to list roles", "err", err)
		relayRBACErr(req, resp, err, "list the roles")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, organizationRolesResponse{Roles: out})
}

func (a *MainAPI) saveOrganizationRole(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	var body rbac.RoleRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("role save requested", "name", body.Name, "rules", len(body.Rules))
	req = identityCtx(req)
	out, err := a.rbac.SaveRole(req.Request.Context(), req.PathParameter("id"), body)
	if err != nil {
		logger.Warn("failed to save the role", "err", err)
		relayRBACErr(req, resp, err, "save the role")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, out)
}

func (a *MainAPI) deleteOrganizationRole(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	req = identityCtx(req)
	found, err := a.rbac.DeleteRole(req.Request.Context(), req.PathParameter("id"), req.PathParameter("name"))
	if err != nil {
		logger.Warn("failed to delete the role", "err", err)
		relayRBACErr(req, resp, err, "delete the role")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "role not found")
		return
	}
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) assignOrganizationRoleFunc(assign bool) restful.RouteFunction {
	return func(req *restful.Request, resp *restful.Response) {
		logger := requesthelper.Logger(req, a.logger)
		var body struct {
			UserID string `json:"user_id"`
		}
		if err := json.NewDecoder(req.Request.Body).Decode(&body); err != nil {
			requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
			return
		}
		req = identityCtx(req)
		found, err := a.rbac.AssignRole(req.Request.Context(),
			req.PathParameter("id"), req.PathParameter("name"), body.UserID, assign)
		if err != nil {
			logger.Warn("failed to change the assignment", "err", err)
			relayRBACErr(req, resp, err, "change the assignment")
			return
		}
		if !found {
			requesthelper.WriteError(req, resp, http.StatusNotFound, "role not found")
			return
		}
		logger.Info("role assignment changed", "user_id", body.UserID, "assigned", assign)
		resp.WriteHeader(http.StatusNoContent)
	}
}

// --- administrator surface: deciding organization requests -----------------

// registerAdminOrganizationRoutes mounts request approval on the admin web
// service, so it inherits the session + ADMIN_EMAIL gate. Approving is the
// one act that creates an isolation boundary, so it is not an owner's to
// make.
func (a *MainAPI) registerAdminOrganizationRoutes(ws *restful.WebService) {
	ws.Route(ws.GET("/organization-requests").
		To(a.adminListOrganizationRequests).
		Param(ws.QueryParameter("status", "pending|approved|rejected")).
		Doc("List organization requests (admin only)").
		Returns(http.StatusOK, "OK", organizationRequestsResponse{}).
		Returns(http.StatusForbidden, "Not the administrator", errorResponse{}))

	for _, decision := range []struct {
		path    string
		approve bool
	}{{"approve", true}, {"reject", false}} {
		ws.Route(ws.POST("/organization-requests/{id}/"+decision.path).
			To(a.adminDecideOrganizationRequestFunc(decision.approve)).
			Consumes(restful.MIME_JSON).
			Param(ws.PathParameter("id", "request id")).
			Doc("Decide an organization request (admin only); approval creates the organization with the requester as its first owner").
			Returns(http.StatusOK, "Decided", rbac.OrganizationRequest{}).
			Returns(http.StatusNotFound, "No such request", errorResponse{}).
			Returns(http.StatusForbidden, "Not the administrator", errorResponse{}))
	}
}

func (a *MainAPI) adminListOrganizationRequests(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("admin_id", sessionAttr(req).UserID)
	req = identityCtx(req)
	out, err := a.rbac.ListOrganizationRequests(req.Request.Context(), req.QueryParameter("status"))
	if err != nil {
		logger.Error("failed to list organization requests", "err", err)
		relayRBACErr(req, resp, err, "list the requests")
		return
	}
	logger.Info("organization requests listed", "count", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, organizationRequestsResponse{Requests: out})
}

func (a *MainAPI) adminDecideOrganizationRequestFunc(approve bool) restful.RouteFunction {
	return func(req *restful.Request, resp *restful.Response) {
		logger := requesthelper.Logger(req, a.logger).WithFields("admin_id", sessionAttr(req).UserID)
		var body struct {
			Reason string `json:"reason,omitempty"`
		}
		_ = json.NewDecoder(req.Request.Body).Decode(&body)
		id := req.PathParameter("id")
		logger.Info("organization request decision", "request_id", id, "approve", approve)

		req = identityCtx(req)
		out, found, err := a.rbac.DecideRequest(req.Request.Context(), id, approve, body.Reason)
		if err != nil {
			logger.Warn("failed to decide the request", "err", err)
			relayRBACErr(req, resp, err, "decide the request")
			return
		}
		if !found {
			requesthelper.WriteError(req, resp, http.StatusNotFound, "request not found")
			return
		}
		logger.Info("organization request decided", "request_id", id, "status", out.Status,
			"organization_id", out.OrganizationID)
		requesthelper.WriteJSON(req, resp, http.StatusOK, out)
	}
}

// errorsAs is errors.As, named locally so the relay reads as one thought.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// relayRBACErr maps RBAC client errors onto user-facing replies, the same way
// relayIdentitiesErr does for the identity service: a refusal and a
// validation message reach the caller intact, and only a genuine failure to
// reach the service becomes a 502.
func relayRBACErr(req *restful.Request, resp *restful.Response, err error, action string) {
	var forbidden *rbac.ForbiddenError
	if errorsAs(err, &forbidden) {
		requesthelper.WriteError(req, resp, http.StatusForbidden, forbidden.Message)
		return
	}
	var badReq *requesthelper.BadRequestError
	if errorsAs(err, &badReq) {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, badReq.Message)
		return
	}
	requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to "+action)
}
