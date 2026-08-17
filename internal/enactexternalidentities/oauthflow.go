package enactexternalidentities

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"golang.org/x/oauth2"

	"enact/internal/extidentities"
	"enact/internal/logging"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// authorizationResponse is what a caller needs to send a browser into the
// provider's consent screen.
type authorizationResponse struct {
	AuthorizationURL string    `json:"authorization_url"`
	State            string    `json:"state"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func (a *IdentitiesAPI) oauthWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1/oauth").Produces(restful.MIME_JSON)

	ws.Route(ws.GET("/authorize").
		To(a.authorize).
		Param(ws.QueryParameter("provider", "provider name").Required(true)).
		Param(ws.QueryParameter("redirect_uri", "where to send the browser once the callback completes")).
		Param(ws.QueryParameter("scopes", "scope to request (repeatable); required unless access_level is given").AllowMultiple(true)).
		Param(ws.QueryParameter("access_level", "a provider-defined access level to request instead of raw scopes")).
		Param(ws.QueryParameter("redirect", "302 to the provider instead of returning the URL").DataType("boolean")).
		Param(ws.QueryParameter("user_id", "override the caller identity (service callers)")).
		Doc("Begin an OAuth authorization: returns (or redirects to) the provider's consent URL. Exactly one of access_level or scopes is required — the provider's configured scopes are not an implicit default").
		Returns(http.StatusOK, "OK", authorizationResponse{}).
		Returns(http.StatusFound, "Redirecting to the provider", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "No such provider", errorResponse{}))

	ws.Route(ws.GET("/callback").
		To(a.callback).
		Param(ws.QueryParameter("code", "authorization code")).
		Param(ws.QueryParameter("state", "the state issued by authorize")).
		Param(ws.QueryParameter("error", "provider-reported failure")).
		Doc("OAuth redirect target: exchanges the code and stores the identity").
		Returns(http.StatusOK, "Stored", identityResponse{}).
		Returns(http.StatusFound, "Redirecting back to the caller", nil).
		Returns(http.StatusBadRequest, "Unknown/expired state or exchange failure", errorResponse{}))

	return ws
}

// redirectURI is the provider-facing callback URL. It must match, byte for
// byte, the value registered in the provider's console — hence deriving it
// from one configured public URL rather than from the request.
func (a *IdentitiesAPI) redirectURI() string {
	return a.publicURL + "/v1/oauth/callback"
}

// mustNameAccess builds the "say what you want" error, naming the
// provider's levels when it defines any so a caller (or a UI) is told what
// it may pick rather than just what it did wrong.
func mustNameAccess(rec extidentities.ProviderRecord) string {
	if len(rec.AccessLevels) > 0 {
		levels := make([]string, 0, len(rec.AccessLevels))
		for name := range rec.AccessLevels {
			levels = append(levels, name)
		}
		sort.Strings(levels)
		return fmt.Sprintf("name what to request: access_level (one of %s) or scopes", strings.Join(levels, ", "))
	}
	return "name what to request: scopes (this provider defines no access levels)"
}

// unionAccess returns requested plus every entry of granted it does not
// already contain, preserving the requested order so the consent URL stays
// stable for an unchanged request.
func unionAccess(requested, granted []string) []string {
	seen := make(map[string]struct{}, len(requested))
	for _, s := range requested {
		seen[s] = struct{}{}
	}
	// Copy: requested is the query's own slice, and appending in place would
	// scribble on it.
	out := append([]string(nil), requested...)
	for _, s := range granted {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func (a *IdentitiesAPI) authorize(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := callerUserID(req)
	providerName := req.QueryParameter("provider")
	scopes := req.Request.URL.Query()["scopes"]
	accessLevel := req.QueryParameter("access_level")
	redirectURI := req.QueryParameter("redirect_uri")
	logger.Info("oauth authorization requested", "user_id", userID, "provider", providerName,
		"scopes", scopes, "access_level", accessLevel, "redirect_uri", redirectURI)

	organizationID, err := a.organizationOf(req.Request.Context(), userID)
	if err != nil {
		logger.Warn("authorize: cannot resolve the user's organization", "user_id", userID, "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	rec, provider, found, err := a.resolveProvider(req, organizationID, providerName)
	if err != nil {
		logger.Error("failed to resolve provider", "provider", providerName, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to resolve provider")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("provider %q not found", providerName))
		return
	}
	oauthProvider, ok := provider.(extidentities.OAuthFlow)
	if !ok {
		logger.Warn("authorize on a non-oauth provider", "provider", providerName, "type", rec.Type)
		requesthelper.WriteError(req, resp, http.StatusBadRequest,
			fmt.Sprintf("provider %q is a %s provider and has no authorization flow; POST /v1/identities instead", providerName, rec.Type))
		return
	}
	// A named level and raw scopes are two ways to say the same thing;
	// asking for both leaves it ambiguous which one the caller meant.
	if accessLevel != "" && len(scopes) > 0 {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "pass either access_level or scopes, not both")
		return
	}
	// What a user is asked to consent to must be stated by the caller. The
	// provider's configured scopes are deliberately NOT an implicit default
	// here: falling back to them would show the user a consent screen for
	// permissions the caller never named, and a later change to the
	// provider record would silently widen every new authorization.
	if accessLevel == "" && len(scopes) == 0 {
		logger.Warn("authorize named neither an access level nor scopes")
		requesthelper.WriteError(req, resp, http.StatusBadRequest, mustNameAccess(rec))
		return
	}
	if accessLevel != "" {
		if _, defined := rec.AccessLevels[accessLevel]; !defined {
			logger.Warn("authorize names an unknown access level", "access_level", accessLevel)
			requesthelper.WriteError(req, resp, http.StatusBadRequest,
				fmt.Sprintf("provider %q defines no access level %q", providerName, accessLevel))
			return
		}
		scopes, _ = extidentities.ResolveAccess(rec, []string{accessLevel})
		logger.Info("access level resolved to scopes", "access_level", accessLevel, "scopes", scopes)
	}

	// One identity per provider means one authorization can narrow what
	// another consumer already relies on: connecting at "read" over an
	// existing "admin" credential would break every tool that needed admin.
	// So ask for what the user already granted as well. The consent screen
	// shows the union, and re-connecting is never a downgrade.
	//
	// Metadata only: this needs the recorded scopes, not the credential. That
	// also means an identity whose envelope can no longer be opened — the
	// very one a user is most likely to be re-authorizing — still widens the
	// request instead of silently losing its access.
	if existing, found, err := a.repo.GetIdentityMetadata(req.Request.Context(), userID, providerName); err != nil {
		logger.Warn("could not read the existing identity; requesting only the named access", "err", err)
	} else if found {
		if widened := unionAccess(scopes, existing.Access); len(widened) > len(scopes) {
			logger.Info("widening the request to keep already-granted access",
				"named", len(scopes), "already_granted", len(existing.Access), "requesting", len(widened))
			scopes = widened
		}
	}

	pending := flow{
		UserID:      userID,
		Provider:    providerName,
		RedirectURI: redirectURI,
		Scopes:      scopes,
		AccessLevel: accessLevel,
	}
	if oauthProvider.UsePKCE() {
		pending.Verifier = oauth2.GenerateVerifier()
	}
	started, err := a.flows.Begin(pending)
	if err != nil {
		logger.Error("failed to start authorization flow", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to start authorization")
		return
	}
	authURL := oauthProvider.AuthCodeURL(started.State, a.redirectURI(), scopes, started.Verifier)
	// The state is a bearer secret for the duration of the flow: log that
	// one was issued, never its value.
	logger.Info("authorization started", "provider", providerName,
		"pkce", started.Verifier != "", "expires_at", started.ExpiresAt, "pending_flows", a.flows.Pending())

	if req.QueryParameter("redirect") == "true" {
		http.Redirect(resp.ResponseWriter, req.Request, authURL, http.StatusFound)
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, authorizationResponse{
		AuthorizationURL: authURL,
		State:            started.State,
		ExpiresAt:        started.ExpiresAt,
	})
}

func (a *IdentitiesAPI) callback(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("oauth callback received")

	// Fail the flow before anything else if the provider reported one.
	if providerErr := req.QueryParameter("error"); providerErr != "" {
		f, _ := a.flows.Take(req.QueryParameter("state"))
		logger.Warn("provider reported an authorization failure", "error", providerErr, "provider", f.Provider)
		a.finishCallback(req, resp, logger, f, http.StatusBadRequest,
			fmt.Sprintf("the provider refused the authorization: %s", providerErr), nil)
		return
	}
	state := req.QueryParameter("state")
	code := req.QueryParameter("code")
	if state == "" || code == "" {
		logger.Warn("callback missing code or state", "has_state", state != "", "has_code", code != "")
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "the callback is missing its code or state")
		return
	}
	// Single-use: a replayed callback finds nothing.
	f, ok := a.flows.Take(state)
	if !ok {
		logger.Warn("callback with unknown or expired state")
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "this authorization link is unknown or has expired; start again")
		return
	}
	logger = logger.WithFields("user_id", f.UserID, "provider", f.Provider)
	logger.Info("callback state resolved")

	organizationID, orgErr := a.organizationOf(req.Request.Context(), f.UserID)
	if orgErr != nil {
		logger.Warn("callback: cannot resolve the user's organization", "err", orgErr)
		a.finishCallback(req, resp, logger, f, http.StatusForbidden, "your account is no longer in an organization", nil)
		return
	}
	rec, provider, found, err := a.resolveProvider(req, organizationID, f.Provider)
	if err != nil || !found {
		logger.Error("failed to resolve provider for callback", "found", found, "err", err)
		a.finishCallback(req, resp, logger, f, http.StatusBadRequest, "the provider is no longer registered", nil)
		return
	}
	oauthProvider, ok := provider.(extidentities.OAuthFlow)
	if !ok {
		a.finishCallback(req, resp, logger, f, http.StatusBadRequest, "the provider has no authorization flow", nil)
		return
	}

	payload, err := oauthProvider.Exchange(req.Request.Context(), code, a.redirectURI(), f.Verifier)
	if err != nil {
		// exchangeError already stripped the provider's response body.
		logger.Warn("code exchange failed", "err", err)
		a.finishCallback(req, resp, logger, f, http.StatusBadRequest, "the authorization code could not be exchanged", nil)
		return
	}
	logger.Info("code exchanged")

	stored, status, msg := a.persistIdentity(req, logger, rec, provider, f.UserID, payload, f.Scopes, f.AccessLevel)
	if status != 0 {
		a.finishCallback(req, resp, logger, f, status, msg, nil)
		return
	}
	a.finishCallback(req, resp, logger, f, http.StatusOK, "", &stored)
}

// finishCallback ends a browser-driven flow: back to the caller's
// redirect_uri when one was given (with connected=1 or an error reason),
// otherwise a plain JSON response for API-driven callers.
func (a *IdentitiesAPI) finishCallback(req *restful.Request, resp *restful.Response, logger *logging.Logger, f flow, status int, msg string, stored *extidentities.Identity) {
	if f.RedirectURI != "" {
		target := f.RedirectURI
		sep := "?"
		if u, err := url.Parse(target); err == nil && u.RawQuery != "" {
			sep = "&"
		}
		if stored != nil {
			target += sep + "connected=1&provider=" + url.QueryEscape(f.Provider)
		} else {
			target += sep + "connected=0&reason=" + url.QueryEscape(msg)
		}
		logger.Info("redirecting after callback", "connected", stored != nil)
		http.Redirect(resp.ResponseWriter, req.Request, target, http.StatusFound)
		return
	}
	if stored == nil {
		requesthelper.WriteError(req, resp, status, msg)
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, toIdentityResponse(*stored))
}
