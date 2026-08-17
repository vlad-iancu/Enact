package enactexternalidentities

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/extidentities"
	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// registerOAuthProviderRequest is what it takes to identify an OAuth
// provider: either a discovery URL (the well-known document) or explicit
// endpoints, plus this platform's client registration with that provider.
type registerOAuthProviderRequest struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	DiscoveryURL string   `json:"discovery_url"`
	Issuer       string   `json:"issuer"`
	AuthorizeURL string   `json:"authorize_url"`
	TokenURL     string   `json:"token_url"`
	RevokeURL    string   `json:"revoke_url"`
	UserInfoURL  string   `json:"userinfo_url"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Scopes       []string `json:"scopes"`
	UsePKCE      bool     `json:"use_pkce"`
	// AccessType "offline" plus Prompt "consent" is what makes most
	// providers issue (and re-issue) a refresh token.
	AccessType      string            `json:"access_type"`
	Prompt          string            `json:"prompt"`
	AuthStyle       string            `json:"auth_style"`
	ExtraAuthParams map[string]string `json:"extra_auth_params"`
	// AccessLevels are named tiers mapping to the scopes each needs, e.g.
	// {"read": ["repo:status","read:org"], "admin": ["repo","admin:org"]}.
	AccessLevels map[string][]string `json:"access_levels"`
}

// registerPATProviderRequest registers a provider whose credentials the
// user pastes in.
type registerPATProviderRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Scheme      string `json:"scheme"`
	HeaderName  string `json:"header_name"`
	DocsURL     string `json:"docs_url"`
	// AccessLevels label what a pasted token is believed to cover; there is
	// no consent screen to enforce them, so they are a vocabulary for
	// callers rather than a guarantee.
	AccessLevels map[string][]string `json:"access_levels"`
}

// providerResponse never echoes the client secret — only whether one is set.
type providerResponse struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	DisplayName     string `json:"display_name,omitempty"`
	AuthorizeURL    string `json:"authorize_url,omitempty"`
	TokenURL        string `json:"token_url,omitempty"`
	Issuer          string `json:"issuer,omitempty"`
	ClientID        string `json:"client_id,omitempty"`
	ClientSecretSet bool   `json:"client_secret_set,omitempty"`
	// CallbackURL is the redirect URI this platform will use — the exact
	// string that must be registered in the provider's console. Derived
	// from IDENTITIES_PUBLIC_URL, so it is right for this deployment.
	CallbackURL  string              `json:"callback_url,omitempty"`
	Scopes       []string            `json:"scopes,omitempty"`
	UsePKCE      bool                `json:"use_pkce,omitempty"`
	Scheme       string              `json:"scheme,omitempty"`
	HeaderName   string              `json:"header_name,omitempty"`
	DocsURL      string              `json:"docs_url,omitempty"`
	AccessLevels map[string][]string `json:"access_levels,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	UpdatedAt    time.Time           `json:"updated_at"`
}

type providersResponse struct {
	Providers []providerResponse `json:"providers"`
}

func (a *IdentitiesAPI) toProviderResponse(rec extidentities.ProviderRecord) providerResponse {
	out := providerResponse{
		Name:         rec.Name,
		Type:         string(rec.Type),
		DisplayName:  rec.DisplayName,
		AccessLevels: rec.AccessLevels,
		CreatedAt:    rec.CreatedAt,
		UpdatedAt:    rec.UpdatedAt,
	}
	if rec.OAuth != nil {
		out.AuthorizeURL = rec.OAuth.AuthorizeURL
		out.TokenURL = rec.OAuth.TokenURL
		out.Issuer = rec.OAuth.Issuer
		out.ClientID = rec.OAuth.ClientID
		out.ClientSecretSet = rec.OAuth.ClientSecretEnc != ""
		out.Scopes = rec.OAuth.Scopes
		out.UsePKCE = rec.OAuth.UsePKCE
		out.CallbackURL = a.redirectURI()
	}
	if rec.PAT != nil {
		out.Scheme = rec.PAT.Scheme
		out.HeaderName = rec.PAT.HeaderName
		out.DocsURL = rec.PAT.DocsURL
	}
	return out
}

func (a *IdentitiesAPI) providersWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1/providers").Produces(restful.MIME_JSON)

	ws.Route(ws.POST("/oauth").
		To(a.registerOAuthProvider).
		Consumes(restful.MIME_JSON).
		Reads(registerOAuthProviderRequest{}).
		Doc("Register an OAuth identity provider; discovery_url is fetched to fill the endpoints when they are not given explicitly").
		Returns(http.StatusCreated, "Registered", providerResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "Provider already registered", errorResponse{}).
		Returns(http.StatusBadGateway, "Discovery document unreachable", errorResponse{}))

	ws.Route(ws.POST("/pat").
		To(a.registerPATProvider).
		Consumes(restful.MIME_JSON).
		Reads(registerPATProviderRequest{}).
		Doc("Register a personal-access-token provider").
		Returns(http.StatusCreated, "Registered", providerResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "Provider already registered", errorResponse{}))

	ws.Route(ws.GET("").
		To(a.listProviders).
		Param(ws.QueryParameter("type", "filter by provider type (pat|oauth)")).
		Doc("List registered identity providers").
		Returns(http.StatusOK, "OK", providersResponse{}))

	ws.Route(ws.GET("/{name}").
		To(a.getProvider).
		Param(ws.PathParameter("name", "provider name")).
		Doc("Get one registered provider").
		Returns(http.StatusOK, "OK", providerResponse{}).
		Returns(http.StatusNotFound, "No such provider", errorResponse{}))

	ws.Route(ws.DELETE("/{name}").
		To(a.deleteProvider).
		Param(ws.PathParameter("name", "provider name")).
		Param(ws.QueryParameter("force", "also delete every identity that references it").DataType("boolean")).
		Doc("Delete a provider; refused while identities still reference it unless force=true, which deletes those identities too").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "No such provider", errorResponse{}).
		Returns(http.StatusConflict, "Identities still reference this provider", errorResponse{}))

	return ws
}

func (a *IdentitiesAPI) registerOAuthProvider(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("oauth provider registration requested")
	if err := a.enforcer.Require(req.Request.Context(), rbac.Permission(rbac.ResourceProvider, rbac.ActionCreate, "*")); err != nil {
		logger.Warn("oauth provider registration denied", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}

	var body registerOAuthProviderRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid oauth provider body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	// The client secret is a credential: log its presence, never its value.
	logger.Info("oauth provider decoded", "name", body.Name, "discovery_url", body.DiscoveryURL,
		"authorize_url", body.AuthorizeURL, "token_url", body.TokenURL, "client_id", body.ClientID,
		"client_secret_present", body.ClientSecret != "", "scopes", body.Scopes,
		"access_levels", levelNames(body.AccessLevels), "use_pkce", body.UsePKCE)

	if err := extidentities.ValidateProviderName(body.Name); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	if body.ClientID == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "client_id is required")
		return
	}
	if err := extidentities.ValidateAccessLevels(body.AccessLevels); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	// An OAuth provider must state what can be asked for. Consent is the
	// whole mechanism: a user is shown a list of permissions and approves
	// it, so a provider with no scopes and no levels describes nothing a
	// caller could request. (A PAT provider is different — the user pastes a
	// token whose scope the platform cannot see or influence, so declaring
	// levels there is optional labelling.)
	//
	// Checked before discovery: a well-known document names the endpoints,
	// never what this platform intends to request.
	for i, scope := range body.Scopes {
		if strings.TrimSpace(scope) == "" {
			requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("scopes[%d] is empty", i))
			return
		}
	}
	if len(body.Scopes) == 0 && len(body.AccessLevels) == 0 {
		logger.Warn("oauth provider declares neither scopes nor access levels")
		requesthelper.WriteError(req, resp, http.StatusBadRequest,
			"an oauth provider must declare what it can request: give scopes, or access_levels naming the scopes each level stands for")
		return
	}
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("register provider: no organization", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	if _, exists, err := a.repo.GetProvider(req.Request.Context(), organizationID, body.Name); err != nil {
		logger.Error("failed to check existing provider", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register provider")
		return
	} else if exists {
		requesthelper.WriteError(req, resp, http.StatusConflict, fmt.Sprintf("provider %q is already registered", body.Name))
		return
	}

	oauthCfg := extidentities.OAuthConfig{
		Issuer:          body.Issuer,
		DiscoveryURL:    body.DiscoveryURL,
		AuthorizeURL:    body.AuthorizeURL,
		TokenURL:        body.TokenURL,
		RevokeURL:       body.RevokeURL,
		UserInfoURL:     body.UserInfoURL,
		ClientID:        body.ClientID,
		Scopes:          body.Scopes,
		UsePKCE:         body.UsePKCE,
		AccessType:      body.AccessType,
		Prompt:          body.Prompt,
		AuthStyle:       body.AuthStyle,
		ExtraAuthParams: body.ExtraAuthParams,
	}
	// Discovery fills whatever the caller did not state explicitly.
	if body.DiscoveryURL != "" && (oauthCfg.AuthorizeURL == "" || oauthCfg.TokenURL == "") {
		doc, err := a.fetchDiscovery(req, body.DiscoveryURL)
		if err != nil {
			logger.Warn("discovery fetch failed", "discovery_url", body.DiscoveryURL, "err", err)
			requesthelper.WriteError(req, resp, http.StatusBadGateway, fmt.Sprintf("could not read the discovery document: %v", err))
			return
		}
		if oauthCfg.AuthorizeURL == "" {
			oauthCfg.AuthorizeURL = doc.AuthorizationEndpoint
		}
		if oauthCfg.TokenURL == "" {
			oauthCfg.TokenURL = doc.TokenEndpoint
		}
		if oauthCfg.Issuer == "" {
			oauthCfg.Issuer = doc.Issuer
		}
		if oauthCfg.UserInfoURL == "" {
			oauthCfg.UserInfoURL = doc.UserinfoEndpoint
		}
		if oauthCfg.RevokeURL == "" {
			oauthCfg.RevokeURL = doc.RevocationEndpoint
		}
		logger.Info("discovery applied", "issuer", oauthCfg.Issuer,
			"authorize_url", oauthCfg.AuthorizeURL, "token_url", oauthCfg.TokenURL)
	}
	if err := validateEndpoint(oauthCfg.AuthorizeURL); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "authorize_url "+err.Error()+" (supply it explicitly or via discovery_url)")
		return
	}
	if err := validateEndpoint(oauthCfg.TokenURL); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "token_url "+err.Error()+" (supply it explicitly or via discovery_url)")
		return
	}

	sealed, err := a.repo.Vault().Seal(body.ClientSecret)
	if err != nil {
		logger.Error("failed to seal client secret", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register provider")
		return
	}
	oauthCfg.ClientSecretEnc = sealed

	now := time.Now().UTC()
	rec := extidentities.ProviderRecord{
		Name:           body.Name,
		OrganizationID: organizationID,
		CreatedBy:      identity.FromContext(req.Request.Context()),
		Type:           extidentities.ProviderTypeOAuth,
		DisplayName:    body.DisplayName,
		OAuth:          &oauthCfg,
		AccessLevels:   body.AccessLevels,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.repo.SaveProvider(req.Request.Context(), rec); err != nil {
		logger.Error("failed to store provider", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register provider")
		return
	}
	if !a.recordProviderOwnership(req, logger, organizationID, rec.Name) {
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to record ownership of the provider")
		return
	}
	logger.Info("oauth provider registered", "name", rec.Name, "authorize_url", oauthCfg.AuthorizeURL,
		"token_url", oauthCfg.TokenURL, "access_levels", levelNames(rec.AccessLevels))
	requesthelper.WriteJSON(req, resp, http.StatusCreated, a.toProviderResponse(rec))
}

func (a *IdentitiesAPI) registerPATProvider(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("pat provider registration requested")
	if err := a.enforcer.Require(req.Request.Context(), rbac.Permission(rbac.ResourceProvider, rbac.ActionCreate, "*")); err != nil {
		logger.Warn("pat provider registration denied", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}

	var body registerPATProviderRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid pat provider body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("pat provider decoded", "name", body.Name, "scheme", body.Scheme,
		"header_name", body.HeaderName, "access_levels", levelNames(body.AccessLevels))

	if err := extidentities.ValidateProviderName(body.Name); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	if err := extidentities.ValidateAccessLevels(body.AccessLevels); err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("register provider: no organization", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	if _, exists, err := a.repo.GetProvider(req.Request.Context(), organizationID, body.Name); err != nil {
		logger.Error("failed to check existing provider", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register provider")
		return
	} else if exists {
		requesthelper.WriteError(req, resp, http.StatusConflict, fmt.Sprintf("provider %q is already registered", body.Name))
		return
	}

	now := time.Now().UTC()
	rec := extidentities.ProviderRecord{
		Name:           body.Name,
		OrganizationID: organizationID,
		CreatedBy:      identity.FromContext(req.Request.Context()),
		Type:           extidentities.ProviderTypePAT,
		DisplayName:    body.DisplayName,
		PAT:            &extidentities.PATConfig{Scheme: body.Scheme, HeaderName: body.HeaderName, DocsURL: body.DocsURL},
		AccessLevels:   body.AccessLevels,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.repo.SaveProvider(req.Request.Context(), rec); err != nil {
		logger.Error("failed to store provider", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register provider")
		return
	}
	if !a.recordProviderOwnership(req, logger, organizationID, rec.Name) {
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to record ownership of the provider")
		return
	}
	logger.Info("pat provider registered", "name", rec.Name)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, a.toProviderResponse(rec))
}

// recordProviderOwnership makes the registrar the owner of the provider they
// just created, exactly as every other service does for the resources it
// creates.
//
// It matters more here than it looks: an organization owner may delegate
// enact:provider:create through a role, and without an ownership rule that
// delegate could register a provider and then be unable to delete it, because
// deletion checks enact:provider:delete:<name>. Owners are unaffected either
// way — they pass by bypass.
//
// Failing to record ownership fails the request: a provider nobody owns is
// worse than no provider, and the caller can retry.
func (a *IdentitiesAPI) recordProviderOwnership(req *restful.Request, logger *logging.Logger, organizationID, name string) bool {
	if err := a.rbac.Grant(req.Request.Context(), rbac.GrantRequest{
		UserID:         identity.FromContext(req.Request.Context()),
		OrganizationID: organizationID,
		Resource:       rbac.ResourceProvider,
		ResourceID:     name,
	}); err != nil {
		logger.Error("failed to record provider ownership", "name", name, "err", err)
		return false
	}
	// The registrar's cached rules predate this grant — see
	// rbac.Enforcer.Forget.
	a.enforcer.Forget(identity.FromContext(req.Request.Context()))
	return true
}

func (a *IdentitiesAPI) listProviders(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	providerType := req.QueryParameter("type")
	logger.Info("list providers requested", "type", providerType)

	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("list providers: no organization", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	records, err := a.repo.ListProviders(req.Request.Context(), organizationID, providerType)
	if err != nil {
		logger.Error("failed to list providers", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list providers")
		return
	}
	out := providersResponse{Providers: make([]providerResponse, 0, len(records))}
	for _, rec := range records {
		out.Providers = append(out.Providers, a.toProviderResponse(rec))
	}
	logger.Info("providers listed", "count", len(out.Providers))
	requesthelper.WriteJSON(req, resp, http.StatusOK, out)
}

func (a *IdentitiesAPI) getProvider(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	name := req.PathParameter("name")
	logger.Info("get provider requested", "name", name)

	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("get provider: no organization", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	rec, found, err := a.repo.GetProvider(req.Request.Context(), organizationID, name)
	if err != nil {
		logger.Error("failed to get provider", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to get provider")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("provider %q not found", name))
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, a.toProviderResponse(rec))
}

func (a *IdentitiesAPI) deleteProvider(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	name := req.PathParameter("name")
	force := req.QueryParameter("force") == "true"
	logger.Info("delete provider requested", "name", name, "force", force)
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceProvider, rbac.ActionDelete, name); err != nil {
		logger.Warn("delete provider denied", "err", err)
		rbac.WriteDenied(req, resp, err, fmt.Sprintf("provider %q not found", name))
		return
	}

	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("delete provider: no organization", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	rec, found, err := a.repo.GetProvider(req.Request.Context(), organizationID, name)
	if err != nil {
		logger.Error("failed to load provider", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete provider")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("provider %q not found", name))
		return
	}
	// Deleting a provider while identities reference it would strand them:
	// no handler could parse their envelope and the sweep would skip them.
	count, err := a.repo.CountByProvider(req.Request.Context(), name)
	if err != nil {
		logger.Error("failed to count referencing identities", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete provider")
		return
	}
	logger.Info("provider reference count", "name", name, "identities", count)
	if count > 0 && !force {
		requesthelper.WriteError(req, resp, http.StatusConflict,
			fmt.Sprintf("%d stored identities still reference provider %q; delete them first or pass force=true", count, name))
		return
	}
	// force takes the identities with it. Identities first: if this fails the
	// provider survives and the call can be retried, whereas the reverse order
	// leaves credentials nobody can open or refresh.
	if count > 0 {
		// Revoke BEFORE anything is deleted: revocation needs this provider
		// record — its revocation endpoint and its client secret — so once
		// the record is gone every one of these grants would stay live with
		// nothing left that could end it. This deletes other people's
		// credentials, which makes it the path where leaving them working is
		// least acceptable.
		records, err := a.repo.ListIdentities(req.Request.Context(), "", name, "")
		if err != nil {
			logger.Error("failed to list the provider's identities", "err", err)
			requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete the provider's stored identities")
			return
		}
		if len(records) < count {
			// ListIdentities caps its page; say so rather than let the log
			// imply everything was revoked.
			logger.Warn("more identities than one page; the remainder will be deleted without revocation",
				"name", name, "identities", count, "listed", len(records))
		}
		tally := a.revokeAll(req, logger, records)
		deleted, err := a.repo.DeleteIdentitiesByProvider(req.Request.Context(), name)
		if err != nil {
			logger.Error("failed to delete referencing identities", "err", err)
			requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete the provider's stored identities")
			return
		}
		logger.Info("forced provider deletion removed identities", "name", name, "identities", deleted,
			"revoked", tally.Revoked, "revocation_unsupported", tally.Unsupported, "revocation_failed", tally.Failed)
	}
	if err := a.repo.DeleteProvider(req.Request.Context(), organizationID, name); err != nil {
		logger.Error("failed to delete provider", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete provider")
		return
	}
	// The ownership rule goes with the record. Warned rather than failed:
	// the provider is already gone, and a stale rule naming a provider that
	// no longer exists grants access to nothing.
	if rec.CreatedBy != "" {
		if err := a.rbac.Revoke(req.Request.Context(), rbac.GrantRequest{
			UserID:         rec.CreatedBy,
			OrganizationID: organizationID,
			Resource:       rbac.ResourceProvider,
			ResourceID:     name,
		}); err != nil {
			logger.Warn("failed to revoke provider ownership; the rule now points at nothing",
				"name", name, "err", err)
		}
		a.enforcer.Forget(rec.CreatedBy)
	}
	logger.Info("provider deleted", "name", name, "identities_deleted", count)
	resp.WriteHeader(http.StatusNoContent)
}

// discoveryDocument is the subset of an OIDC/OAuth well-known document the
// platform needs.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

func (a *IdentitiesAPI) fetchDiscovery(req *restful.Request, discoveryURL string) (discoveryDocument, error) {
	var doc discoveryDocument
	if err := validateEndpoint(discoveryURL); err != nil {
		return doc, fmt.Errorf("discovery_url %s", err)
	}
	httpReq, err := http.NewRequestWithContext(req.Request.Context(), http.MethodGet, discoveryURL, nil)
	if err != nil {
		return doc, err
	}
	resp, err := a.providerHTTP.Do(httpReq)
	if err != nil {
		return doc, fmt.Errorf("unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return doc, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return doc, fmt.Errorf("not a valid discovery document")
	}
	return doc, nil
}

func validateEndpoint(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be a valid http(s) URL")
	}
	return nil
}

// levelNames lists the defined access level names for logging; the scope
// lists themselves are noise in a log line.
func levelNames(levels map[string][]string) []string {
	if len(levels) == 0 {
		return nil
	}
	out := make([]string, 0, len(levels))
	for name := range levels {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
