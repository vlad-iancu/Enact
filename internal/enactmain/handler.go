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

	"enact/internal/agents"
	"enact/internal/conversations"
	"enact/internal/inference"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/models"
	"enact/internal/requesthelper"
	"enact/internal/users"
)

// minPasswordLen is the minimum accepted password length for local accounts.
const minPasswordLen = 8

// MainAPI is the UI backend: Google OAuth login, local email+password
// accounts, and cookie sessions.
type MainAPI struct {
	users         *users.Repository
	sessions      *SessionStore
	google        *googleAuth
	conversations *conversations.Repository
	inference     *inference.Client
	models        *models.Client
	agents        *agents.Client
	kb            *kb.Client
	cookies       cookieSettings
	// frontendURL, when set, is where browser flows land after auth (an
	// external SPA origin); empty means the built-in pages.
	frontendURL string
	logger      *logging.Logger
}

func newMainAPI(userRepo *users.Repository, sessions *SessionStore, google *googleAuth, convRepo *conversations.Repository, inferenceClient *inference.Client, modelsClient *models.Client, agentsClient *agents.Client, kbClient *kb.Client, cookies cookieSettings, frontendURL string, logger *logging.Logger) *MainAPI {
	return &MainAPI{
		users:         userRepo,
		sessions:      sessions,
		google:        google,
		conversations: convRepo,
		inference:     inferenceClient,
		models:        modelsClient,
		agents:        agentsClient,
		kb:            kbClient,
		cookies:       cookies,
		frontendURL:   strings.TrimRight(frontendURL, "/"),
		logger:        logger,
	}
}

// postLoginTarget is where a completed browser login lands: the external
// frontend when configured, else the built-in app page.
func (a *MainAPI) postLoginTarget() string {
	if a.frontendURL != "" {
		return a.frontendURL
	}
	return "/app"
}

// csrfOriginFilter rejects state-changing requests (POST/PUT/DELETE) whose
// Origin header names a third party. Browsers always attach Origin to
// cross-origin mutation requests, so a forged request carries the attacker
// site's origin; the
// frontend origin and this service's own origin (built-in pages) are the
// only accepted values. Requests without an Origin header (curl, native
// clients, some same-origin cases) pass — this guards browsers, and cookie
// auth is what makes browser CSRF dangerous. This matters most with
// SameSite=None cookies, where the cookie itself no longer blocks CSRF.
func (a *MainAPI) csrfOriginFilter(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	r := req.Request
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
		if origin := r.Header.Get("Origin"); origin != "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			self := scheme + "://" + r.Host
			if origin != a.frontendURL && origin != self {
				requesthelper.Logger(req, a.logger).Warn("cross-origin post rejected", "origin", origin)
				requesthelper.WriteError(req, resp, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		}
	}
	chain.ProcessFilter(req, resp)
}

type registerRequest struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// userResponse is the public shape of an account: exactly the fields the
// platform cares about, never the password hash.
type userResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	EmailVerified bool   `json:"email_verified"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func toUserResponse(u users.User) userResponse {
	return userResponse{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName, EmailVerified: u.EmailVerified}
}

// WebServices returns the route groups. Distinct root paths are separate
// go-restful WebServices; the callback path is fixed by the redirect URI
// registered in the Google console.
func (a *MainAPI) WebServices() []*restful.WebService {
	auth := new(restful.WebService)
	auth.Path("/auth").Produces(restful.MIME_JSON)
	// Origin validation on the state-changing routes (CSRF defense; see the
	// filter). Registered on the auth group only — the other groups are
	// GET-only pages and the OAuth callback, which state+PKCE protect.
	auth.Filter(a.csrfOriginFilter)

	auth.Route(auth.GET("/google").
		To(a.googleLogin).
		Doc("Begin the Login-with-Google flow: sets the state+PKCE cookie and redirects to Google's consent page"))

	auth.Route(auth.POST("/register").
		To(a.register).
		Consumes(restful.MIME_JSON).
		Reads(registerRequest{}).
		Doc("Register a local account (email + password) and log it in").
		Returns(http.StatusCreated, "Registered", userResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "Email already registered", errorResponse{}))

	auth.Route(auth.POST("/login").
		To(a.login).
		Consumes(restful.MIME_JSON).
		Reads(loginRequest{}).
		Doc("Log in with email + password; sets the session cookie").
		Returns(http.StatusOK, "Logged in", userResponse{}).
		Returns(http.StatusUnauthorized, "Bad credentials", errorResponse{}).
		Returns(http.StatusForbidden, "Email not verified", errorResponse{}))

	auth.Route(auth.POST("/logout").
		To(a.logout).
		Doc("Destroy the session and clear the cookie").
		Returns(http.StatusNoContent, "Logged out", nil))

	auth.Route(auth.GET("/me").
		To(a.me).
		Doc("Return the logged-in user").
		Returns(http.StatusOK, "OK", userResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	callback := new(restful.WebService)
	callback.Path("/google")
	callback.Route(callback.GET("/oauth/callback").
		To(a.googleCallback).
		Doc("Google OAuth redirect target: verifies state, exchanges the code (PKCE), validates the ID token, upserts the user, starts a session"))

	app := new(restful.WebService)
	app.Path("/app")
	app.Route(app.GET("").
		To(a.appPage).
		Produces("text/html").
		Doc("The (placeholder) UI homepage; requires a session, else redirects to /login"))

	login := new(restful.WebService)
	login.Path("/login")
	login.Route(login.GET("").
		To(a.loginPage).
		Produces("text/html").
		Doc("The login page: Google button plus local login/register forms"))

	return []*restful.WebService{
		auth, callback, app, login,
		a.conversationsWebService(), a.modelsWebService(),
		a.agentsWebService(), a.kbWebService(),
	}
}

// session resolves the request's session cookie, if any.
func (a *MainAPI) session(req *restful.Request) (Session, bool) {
	cookie, err := req.Request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return Session{}, false
	}
	return a.sessions.Get(cookie.Value)
}

// ---------------------------------------------------------------------------
// Google OAuth
// ---------------------------------------------------------------------------

func (a *MainAPI) googleLogin(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("google login requested")
	authURL, err := a.google.begin(resp.ResponseWriter, a.cookies.Secure)
	if err != nil {
		logger.Error("failed to begin google flow", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to begin login flow")
		return
	}
	logger.Info("redirecting to google consent")
	http.Redirect(resp.ResponseWriter, req.Request, authURL, http.StatusFound)
}

func (a *MainAPI) googleCallback(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("google callback received")

	identity, err := a.google.finish(req.Request, resp.ResponseWriter, a.cookies.Secure)
	if err != nil {
		logger.Warn("google flow failed", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("login failed: %v", err))
		return
	}
	logger.Info("google identity validated", "email", identity.Email, "email_verified", identity.EmailVerified)

	// The platform authorizes only verified email addresses.
	if !identity.EmailVerified {
		logger.Warn("google email not verified", "email", identity.Email)
		requesthelper.WriteError(req, resp, http.StatusForbidden, "your Google account's email address is not verified")
		return
	}

	user, err := a.upsertGoogleUser(req, identity)
	if err != nil {
		logger.Error("failed to upsert google user", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to store user")
		return
	}
	logger.Info("user upserted", "user_id", user.ID)

	sess, err := a.sessions.Create(user.ID, user.Email, user.DisplayName)
	if err != nil {
		logger.Error("failed to create session", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create session")
		return
	}
	setSessionCookie(resp.ResponseWriter, sess, a.cookies)
	logger.Info("session created; redirecting to app", "user_id", user.ID)
	http.Redirect(resp.ResponseWriter, req.Request, a.postLoginTarget(), http.StatusFound)
}

// upsertGoogleUser links a validated Google identity to an account: an
// existing account (by email) gains the Google subject; a new one is
// created. Email verification is a fact asserted by Google's ID token.
func (a *MainAPI) upsertGoogleUser(req *restful.Request, identity googleIdentity) (users.User, error) {
	ctx := req.Request.Context()
	now := time.Now().UTC()

	user, found, err := a.users.GetByEmail(ctx, identity.Email)
	if err != nil {
		return users.User{}, err
	}
	if !found {
		user = users.User{
			ID:          uuid.NewString(),
			Email:       users.NormalizeEmail(identity.Email),
			DisplayName: identity.Name,
			CreatedAt:   now,
		}
	}
	if user.DisplayName == "" {
		user.DisplayName = identity.Name
	}
	user.GoogleSub = identity.Sub
	user.EmailVerified = true
	user.UpdatedAt = now
	if err := a.users.Save(ctx, user); err != nil {
		return users.User{}, err
	}
	return user, nil
}

// ---------------------------------------------------------------------------
// Local accounts
// ---------------------------------------------------------------------------

func (a *MainAPI) register(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("registration requested")

	var body registerRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid registration body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	email := users.NormalizeEmail(body.Email)
	logger.Info("registration decoded", "email", email, "display_name", body.DisplayName, "password_chars", len(body.Password))

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
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register")
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
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register")
		return
	}
	logger.Info("password hashed")

	now := time.Now().UTC()
	user := users.User{
		ID:           uuid.NewString(),
		Email:        email,
		DisplayName:  body.DisplayName,
		PasswordHash: string(hash),
		// No verification-email infrastructure exists; local registrations
		// are trusted. Replace with a real verification flow before any
		// non-local deployment.
		EmailVerified: true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := a.users.Save(req.Request.Context(), user); err != nil {
		logger.Error("failed to store user", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register")
		return
	}
	logger.Info("user registered", "user_id", user.ID)

	sess, err := a.sessions.Create(user.ID, user.Email, user.DisplayName)
	if err != nil {
		logger.Error("failed to create session", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create session")
		return
	}
	setSessionCookie(resp.ResponseWriter, sess, a.cookies)
	logger.Info("session created", "user_id", user.ID)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, toUserResponse(user))
}

func (a *MainAPI) login(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("login requested")

	var body loginRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid login body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	email := users.NormalizeEmail(body.Email)
	logger.Info("login decoded", "email", email)

	user, found, err := a.users.GetByEmail(req.Request.Context(), email)
	if err != nil {
		logger.Error("failed to look up user", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to log in")
		return
	}
	// Run the comparison even for unknown users (against an empty hash) so
	// response timing does not reveal which emails exist.
	hash := ""
	if found {
		hash = user.PasswordHash
	}
	compareErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password))
	if !found || hash == "" || compareErr != nil {
		logger.Warn("login rejected", "email", email, "known_user", found, "has_password", hash != "")
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "invalid email or password")
		return
	}
	logger.Info("password verified", "user_id", user.ID)

	// The platform authorizes only verified email addresses.
	if !user.EmailVerified {
		logger.Warn("login rejected: email not verified", "user_id", user.ID)
		requesthelper.WriteError(req, resp, http.StatusForbidden, "email address is not verified")
		return
	}

	sess, err := a.sessions.Create(user.ID, user.Email, user.DisplayName)
	if err != nil {
		logger.Error("failed to create session", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to log in")
		return
	}
	setSessionCookie(resp.ResponseWriter, sess, a.cookies)
	logger.Info("session created", "user_id", user.ID)
	requesthelper.WriteJSON(req, resp, http.StatusOK, toUserResponse(user))
}

// ---------------------------------------------------------------------------
// Session endpoints and pages
// ---------------------------------------------------------------------------

func (a *MainAPI) logout(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	if cookie, err := req.Request.Cookie(sessionCookie); err == nil {
		a.sessions.Delete(cookie.Value)
	}
	clearSessionCookie(resp.ResponseWriter, a.cookies)
	logger.Info("logged out")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) me(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	sess, ok := a.session(req)
	if !ok {
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "not logged in")
		return
	}
	logger.Info("session resolved", "user_id", sess.UserID)
	user, found, err := a.users.GetByEmail(req.Request.Context(), sess.Email)
	if err != nil || !found {
		logger.Error("failed to load session user", "user_id", sess.UserID, "found", found, "err", err)
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "not logged in")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, toUserResponse(user))
}

// appPage is the session-guarded homepage: with a session it renders, else
// it redirects back to the login page.
func (a *MainAPI) appPage(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	sess, ok := a.session(req)
	if !ok {
		logger.Info("no session; redirecting to login")
		http.Redirect(resp.ResponseWriter, req.Request, "/login", http.StatusFound)
		return
	}
	logger.Info("rendering app page", "user_id", sess.UserID)
	renderAppPage(resp.ResponseWriter, sess)
}

func (a *MainAPI) loginPage(req *restful.Request, resp *restful.Response) {
	// An already-authenticated visitor goes straight to the app.
	if _, ok := a.session(req); ok {
		http.Redirect(resp.ResponseWriter, req.Request, a.postLoginTarget(), http.StatusFound)
		return
	}
	renderLoginPage(resp.ResponseWriter)
}
