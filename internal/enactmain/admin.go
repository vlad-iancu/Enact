package enactmain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"enact/internal/requesthelper"
	"enact/internal/users"
)

// adminWebService groups the administrator-only user-management endpoints.
// Access requires a session belonging to the ADMIN_EMAIL account; there is
// no role system — the administrator is designated by configuration.
func (a *MainAPI) adminWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/admin").Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireSession)
	ws.Filter(a.requireAdmin)

	ws.Route(ws.POST("/create-user").
		To(a.adminCreateUser).
		Consumes(restful.MIME_JSON).
		Reads(adminCreateUserRequest{}).
		Doc("Create a local account (admin only); the account is verified immediately, no email is sent").
		Returns(http.StatusCreated, "Created", userResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "Email already registered", errorResponse{}).
		Returns(http.StatusForbidden, "Not the administrator", errorResponse{}))

	ws.Route(ws.POST("/delete-user").
		To(a.adminDeleteUser).
		Consumes(restful.MIME_JSON).
		Reads(adminDeleteUserRequest{}).
		Doc("Delete an account by email (admin only); revokes its sessions and removes its avatar").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "No such user", errorResponse{}).
		Returns(http.StatusForbidden, "Not the administrator", errorResponse{}))

	return ws
}

// requireAdmin gates a route group to the ADMIN_EMAIL account. It runs
// after requireSession, so a session is already attached.
func (a *MainAPI) requireAdmin(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	sess := sessionAttr(req)
	if !a.isAdmin(sess.Email) {
		requesthelper.Logger(req, a.logger).Warn("admin route rejected", "user_id", sess.UserID)
		requesthelper.WriteError(req, resp, http.StatusForbidden, "administrator access required")
		return
	}
	chain.ProcessFilter(req, resp)
}

type adminCreateUserRequest struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

type adminDeleteUserRequest struct {
	Email string `json:"email"`
}

// adminCreateUser creates a local account on the administrator's authority:
// it is verified immediately (the admin vouches for it) and no verification
// email is sent.
func (a *MainAPI) adminCreateUser(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("admin create-user requested", "admin_id", sessionAttr(req).UserID)

	var body adminCreateUserRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create-user body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	email := users.NormalizeEmail(body.Email)
	logger.Info("create-user decoded", "email", email, "display_name", body.DisplayName, "password_chars", len(body.Password))

	if email == "" || !strings.Contains(email, "@") {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "a valid email is required")
		return
	}
	if body.DisplayName == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "display_name is required")
		return
	}
	if len(body.Password) < minPasswordLen {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("password must be at least %d characters", minPasswordLen))
		return
	}

	_, exists, err := a.users.GetByEmail(req.Request.Context(), email)
	if err != nil {
		logger.Error("failed to check existing user", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create user")
		return
	}
	if exists {
		logger.Warn("email already registered", "email", email)
		requesthelper.WriteError(req, resp, http.StatusConflict, "an account with this email already exists")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		logger.Error("failed to hash password", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create user")
		return
	}

	now := time.Now().UTC()
	user := users.User{
		ID:           uuid.NewString(),
		Email:        email,
		DisplayName:  body.DisplayName,
		PasswordHash: string(hash),
		// The administrator vouches for the address: verified from birth,
		// so the account can log in immediately.
		EmailVerified: true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := a.users.Save(req.Request.Context(), user); err != nil {
		logger.Error("failed to store user", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create user")
		return
	}
	logger.Info("user created by admin", "user_id", user.ID, "email", user.Email)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, a.toUserResponse(user))
}

// adminDeleteUser removes an account by email: its sessions are revoked,
// its avatar object deleted best-effort, then the record itself removed.
// The administrator cannot delete their own account (the platform would be
// left without one).
func (a *MainAPI) adminDeleteUser(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("admin delete-user requested", "admin_id", sessionAttr(req).UserID)

	var body adminDeleteUserRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid delete-user body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	email := users.NormalizeEmail(body.Email)
	logger.Info("delete-user decoded", "email", email)

	if email == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "email is required")
		return
	}
	if a.isAdmin(email) {
		logger.Warn("admin attempted self-deletion")
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "the administrator account cannot be deleted")
		return
	}

	user, found, err := a.users.GetByEmail(req.Request.Context(), email)
	if err != nil {
		logger.Error("failed to look up user", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete user")
		return
	}
	if !found {
		logger.Warn("delete-user for unknown email", "email", email)
		requesthelper.WriteError(req, resp, http.StatusNotFound, "no account with this email")
		return
	}

	revoked := a.sessions.DeleteByUserID(user.ID)
	logger.Info("sessions revoked", "user_id", user.ID, "count", revoked)

	// Avatar cleanup is best-effort, like avatar replacement: an orphaned
	// object must never block the account deletion.
	if user.AvatarKey != "" {
		if err := a.storage.DeleteObject(req.Request.Context(), user.AvatarKey); err != nil {
			logger.Warn("failed to delete avatar object", "key", user.AvatarKey, "err", err)
		} else {
			logger.Info("avatar object deleted", "key", user.AvatarKey)
		}
	}

	if err := a.users.Delete(req.Request.Context(), email); err != nil {
		logger.Error("failed to delete user record", "user_id", user.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete user")
		return
	}
	logger.Info("user deleted by admin", "user_id", user.ID, "email", email)
	resp.WriteHeader(http.StatusNoContent)
}
