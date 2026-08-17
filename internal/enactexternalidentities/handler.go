package enactexternalidentities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/extidentities"
	"enact/internal/identity"
	"enact/internal/identityevents"
	"enact/internal/logging"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// IdentitiesAPI serves provider registration, identity storage/retrieval,
// and the OAuth consent flow.
type IdentitiesAPI struct {
	repo  *extidentities.Repository
	flows *flowStore
	// providerHTTP talks to third parties: traced, but never S2S-signed —
	// they are not platform services.
	providerHTTP *http.Client
	events       *identityevents.Publisher
	// enforcer gates PROVIDER management. The per-user identity routes are
	// not gated by rules: an identity IS its user, and the only way to reach
	// someone else's is the service-only user_id override, which the S2S ACL
	// already restricts. The refresh sweep serves no caller and is likewise
	// ungated by construction.
	enforcer *rbac.Enforcer
	// rbac records who owns a provider. The enforcer answers "may they?";
	// this writes "they do", which is a different call and a different
	// lifecycle — see registerOAuth and deleteProvider.
	rbac          *rbac.Client
	refreshWindow time.Duration
	publicURL     string
	logger        *logging.Logger
}

type errorResponse struct {
	Error string `json:"error"`
}

// identityResponse is an identity WITHOUT its credentials. The domain
// record is never serialized directly: it carries the ciphertext.
type identityResponse struct {
	ID           string     `json:"id"`
	UserID       string     `json:"user_id"`
	Provider     string     `json:"provider"`
	ProviderType string     `json:"provider_type"`
	Access       []string   `json:"access,omitempty"`
	AccessLevel  string     `json:"access_level,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Refreshable  bool       `json:"refreshable"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type identitiesResponse struct {
	Identities []identityResponse `json:"identities"`
}

// deletedIdentitiesResponse reports a bulk delete. The revocation counts are
// separate from the delete count on purpose: the platform always forgets the
// credential, but whether the provider was told is a different question, and
// an operator deleting an account needs to see the difference.
type deletedIdentitiesResponse struct {
	Deleted               int `json:"deleted"`
	Revoked               int `json:"revoked"`
	RevocationUnsupported int `json:"revocation_unsupported"`
	RevocationFailed      int `json:"revocation_failed"`
}

// storeIdentityRequest stores a credential the caller already holds.
type storeIdentityRequest struct {
	// UserID is honored only when the caller omits it from the identity
	// header; enact-main always sends the header.
	UserID      string `json:"user_id,omitempty"`
	Provider    string `json:"provider"`
	Credentials any    `json:"credentials"`
	// AccessLevel names a provider-defined level; its scopes are recorded
	// when Access is not given explicitly.
	AccessLevel string   `json:"access_level,omitempty"`
	Access      []string `json:"access,omitempty"`
}

func toIdentityResponse(rec extidentities.Identity) identityResponse {
	return identityResponse{
		ID:           rec.ID,
		UserID:       rec.UserID,
		Provider:     rec.Provider,
		ProviderType: string(rec.ProviderType),
		Access:       rec.Access,
		AccessLevel:  rec.AccessLevel,
		ExpiresAt:    rec.ExpiresAt,
		Refreshable:  rec.Refreshable,
		Status:       rec.Status,
		CreatedAt:    rec.CreatedAt,
		UpdatedAt:    rec.UpdatedAt,
	}
}

// WebServices returns the route groups: identities, providers, and the
// OAuth flow (separate paths keep the ACL readable).
func (a *IdentitiesAPI) WebServices() []*restful.WebService {
	identities := new(restful.WebService)
	identities.Path("/v1/identities").Produces(restful.MIME_JSON)

	identities.Route(identities.POST("").
		To(a.storeIdentity).
		Consumes(restful.MIME_JSON).
		Reads(storeIdentityRequest{}).
		Doc("Store a credential for (user, provider); re-storing the same pair overwrites").
		Returns(http.StatusCreated, "Stored", identityResponse{}).
		Returns(http.StatusBadRequest, "Invalid request or unknown provider", errorResponse{}))

	identities.Route(identities.GET("").
		To(a.listIdentities).
		Param(identities.QueryParameter("provider", "filter by provider name")).
		Param(identities.QueryParameter("status", "filter by status")).
		Param(identities.QueryParameter("user_id", "override the caller identity (service callers)")).
		Doc("List identities (metadata only — never credentials)").
		Returns(http.StatusOK, "OK", identitiesResponse{}))

	identities.Route(identities.GET("/credentials").
		To(a.retrieveCredentials).
		Param(identities.QueryParameter("provider", "provider name").Required(true)).
		Param(identities.QueryParameter("required_access", "scope the credential must cover (repeatable)").AllowMultiple(true)).
		Param(identities.QueryParameter("user_id", "override the caller identity (service callers)")).
		Doc("Return the usable credential for (user, provider)").
		Returns(http.StatusOK, "OK", extidentities.Credential{}).
		Returns(http.StatusNotFound, "No such identity", errorResponse{}).
		Returns(http.StatusConflict, "Revoked, or the grant does not cover required_access", errorResponse{}).
		Returns(http.StatusFailedDependency, "The credential's provider is no longer registered", errorResponse{}))

	identities.Route(identities.DELETE("").
		To(a.deleteIdentity).
		Param(identities.QueryParameter("provider", "provider name").Required(true)).
		Param(identities.QueryParameter("user_id", "override the caller identity (service callers)")).
		Doc("Delete a stored identity, revoking it at the provider first when the provider supports revocation").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "No such identity", errorResponse{}))

	identities.Route(identities.DELETE("/all").
		To(a.deleteAllIdentities).
		Param(identities.QueryParameter("user_id", "override the caller identity (service callers)")).
		Doc("Delete every stored identity of one user, revoking each at its provider — the account-deletion path").
		Returns(http.StatusOK, "Deleted", deletedIdentitiesResponse{}).
		Returns(http.StatusBadRequest, "No user identified", errorResponse{}))

	return []*restful.WebService{identities, a.providersWebService(), a.oauthWebService()}
}

// callerUserID resolves whose credentials a request concerns: the identity
// header, with an explicit user_id override for service callers acting on
// someone's behalf.
//
// NOTE: the header is impersonation, not authentication (see internal/
// identity) — the ACL keeps these routes service-only and enact-main scopes
// them to the session.
func callerUserID(req *restful.Request) string {
	if explicit := req.QueryParameter("user_id"); explicit != "" {
		return explicit
	}
	return identity.FromContext(req.Request.Context())
}

// organizationOf resolves which organization a user's providers come from.
// Provider names are unique only inside an organization, so every provider
// lookup on an identity path needs the credential owner's — not the
// caller's, which for a service-side call is not the same person.
func (a *IdentitiesAPI) organizationOf(ctx context.Context, userID string) (string, error) {
	effective, err := a.enforcer.Effective(ctx, userID)
	if err != nil {
		return "", err
	}
	return effective.OrganizationID, nil
}

// organizationForIdentity prefers the organization stamped on the record and
// falls back to the owner's membership, so a credential written before the
// field existed still resolves its provider.
func (a *IdentitiesAPI) organizationForIdentity(ctx context.Context, record extidentities.Identity) (string, error) {
	if record.OrganizationID != "" {
		return record.OrganizationID, nil
	}
	return a.organizationOf(ctx, record.UserID)
}

// resolveProvider loads an organization's provider record and builds its
// handler.
func (a *IdentitiesAPI) resolveProvider(req *restful.Request, organizationID, name string) (extidentities.ProviderRecord, extidentities.Provider, bool, error) {
	rec, found, err := a.repo.GetProvider(req.Request.Context(), organizationID, name)
	if err != nil || !found {
		return extidentities.ProviderRecord{}, nil, found, err
	}
	provider, err := extidentities.NewProvider(rec, a.repo.Vault(), a.providerHTTP)
	if err != nil {
		return rec, nil, true, err
	}
	return rec, provider, true, nil
}

func (a *IdentitiesAPI) storeIdentity(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("store identity requested")

	var body storeIdentityRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid store body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	userID := body.UserID
	if userID == "" {
		userID = callerUserID(req)
	}
	// Credential values are never logged — only their presence (ADR-0008).
	logger.Info("store decoded", "user_id", userID, "provider", body.Provider,
		"access", body.Access, "access_level", body.AccessLevel,
		"credentials_present", body.Credentials != nil)

	organizationID, err := a.organizationOf(req.Request.Context(), userID)
	if err != nil {
		logger.Warn("store: cannot resolve the user's organization", "user_id", userID, "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	rec, provider, found, err := a.resolveProvider(req, organizationID, body.Provider)
	if err != nil {
		logger.Error("failed to resolve provider", "provider", body.Provider, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to resolve provider")
		return
	}
	if !found {
		logger.Warn("store for unknown provider", "provider", body.Provider)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("provider %q is not registered", body.Provider))
		return
	}

	// A caller may name a level, list raw scopes, or both; ResolveAccess
	// expands levels into the concrete scopes that are actually stored.
	terms := body.Access
	if body.AccessLevel != "" {
		if _, defined := rec.AccessLevels[body.AccessLevel]; !defined {
			logger.Warn("store names an unknown access level", "access_level", body.AccessLevel)
			requesthelper.WriteError(req, resp, http.StatusBadRequest,
				fmt.Sprintf("provider %q defines no access level %q", rec.Name, body.AccessLevel))
			return
		}
		terms = append(terms, body.AccessLevel)
	}
	requested, level := extidentities.ResolveAccess(rec, terms)

	stored, status, msg := a.persistIdentity(req, logger, rec, provider, userID, body.Credentials, requested, level)
	if status != 0 {
		requesthelper.WriteError(req, resp, status, msg)
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusCreated, toIdentityResponse(stored))
}

// persistIdentity runs a payload through the provider and saves the result,
// preserving anything the provider carries forward from an existing
// envelope (notably a refresh token the new payload omits). It returns
// (identity, 0, "") on success or an HTTP status and message on failure.
func (a *IdentitiesAPI) persistIdentity(req *restful.Request, logger *logging.Logger, rec extidentities.ProviderRecord, provider extidentities.Provider, userID string, payload any, requested []string, accessLevel string) (extidentities.Identity, int, string) {
	ctx := req.Request.Context()

	// Note what does NOT happen here: replacing a credential does not revoke
	// the one it replaces. Most providers treat revocation as ending the
	// whole grant, and a re-authorization often returns the same refresh
	// token — so revoking the old value would kill the new one. Revocation
	// belongs to deletion (revokeAll), where the platform is giving the
	// credential up rather than renewing it.
	previousEnvelope := ""
	existing, envelope, found, err := a.repo.GetIdentity(ctx, userID, rec.Name)
	if err != nil {
		// An unreadable existing record must not block re-storing a working
		// credential; it will be overwritten.
		logger.Warn("existing identity could not be opened; overwriting", "err", err)
	} else if found {
		previousEnvelope = envelope
	}

	stored, err := provider.StoreIdentity(ctx, payload, previousEnvelope)
	if err != nil {
		logger.Warn("provider rejected the credentials payload", "provider", rec.Name, "err", err)
		return extidentities.Identity{}, http.StatusBadRequest, err.Error()
	}
	// What the provider says it granted is authoritative. A user can
	// decline a permission at the consent screen, and recording the
	// REQUESTED scopes instead would make later required_access checks pass
	// for access the credential does not actually carry — sending consumers
	// off to make calls that 403. Requested scopes are a fallback only for
	// providers that report no scope at all (and for PATs, where nobody
	// reports anything).
	if len(requested) > 0 {
		if stored.AccessFromProvider {
			if missing := missingAccess(stored.Access, requested); len(missing) > 0 {
				logger.Warn("provider granted less than requested; recording what was granted",
					"granted", stored.Access, "requested", requested, "not_granted", missing)
			}
		} else {
			stored.Access = requested
		}
	}
	// A level only labels the credential if the scopes it stands for were
	// actually granted.
	if accessLevel != "" {
		if defined, ok := rec.AccessLevels[accessLevel]; ok && !extidentities.HasAccess(stored.Access, defined) {
			logger.Warn("access level not fully granted; storing without the level label",
				"access_level", accessLevel, "granted", stored.Access)
			accessLevel = ""
		}
	}

	now := time.Now().UTC()
	record := extidentities.Identity{
		ID:     extidentities.DocID(userID, rec.Name),
		UserID: userID,
		// Taken from the provider record rather than looked up again: it is
		// by definition the organization whose provider produced this
		// credential.
		OrganizationID: rec.OrganizationID,
		Provider:       rec.Name,
		ProviderType:   rec.Type,
		Access:         stored.Access,
		AccessLevel:    accessLevel,
		ExpiresAt:      stored.ExpiresAt,
		Refreshable:    stored.Refreshable,
		Status:         extidentities.StatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if found {
		record.CreatedAt = existing.CreatedAt
	}
	if err := a.repo.SaveIdentity(ctx, record, stored.Envelope); err != nil {
		logger.Error("failed to store identity", "err", err)
		return extidentities.Identity{}, http.StatusInternalServerError, "failed to store identity"
	}
	logger.Info("identity stored", "id", record.ID, "user_id", userID, "provider", rec.Name,
		"access", record.Access, "access_level", record.AccessLevel,
		"access_from_provider", stored.AccessFromProvider, "refreshable", record.Refreshable, "expires_at", record.ExpiresAt)

	// Wake anything parked on this credential — a tool call waiting for the
	// user to connect this very account. Fire and forget: a publish failure
	// must never fail a store, and waiters re-check on a timer anyway.
	// WithoutCancel because on the OAuth path this request's context dies
	// with the browser redirect that is already in flight.
	if a.events != nil {
		pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		if err := a.events.Publish(pubCtx, identityevents.Event{
			Type:        identityevents.TypeIdentityStored,
			UserID:      userID,
			Provider:    rec.Name,
			AccessLevel: record.AccessLevel,
			Access:      record.Access,
			At:          now,
		}); err != nil {
			logger.Warn("failed to publish identity event", "err", err)
		}
		cancel()
	}
	return record, 0, ""
}

// missingAccess returns the required entries granted does not cover.
func missingAccess(granted, required []string) []string {
	have := make(map[string]bool, len(granted))
	for _, g := range granted {
		have[g] = true
	}
	var missing []string
	for _, r := range required {
		if !have[r] {
			missing = append(missing, r)
		}
	}
	return missing
}

func (a *IdentitiesAPI) listIdentities(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := callerUserID(req)
	provider := req.QueryParameter("provider")
	status := req.QueryParameter("status")
	logger.Info("list identities requested", "user_id", userID, "provider", provider, "status", status)

	records, err := a.repo.ListIdentities(req.Request.Context(), userID, provider, status)
	if err != nil {
		logger.Error("failed to list identities", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list identities")
		return
	}
	out := identitiesResponse{Identities: make([]identityResponse, 0, len(records))}
	for _, rec := range records {
		out.Identities = append(out.Identities, toIdentityResponse(rec))
	}
	logger.Info("identities listed", "count", len(out.Identities))
	requesthelper.WriteJSON(req, resp, http.StatusOK, out)
}

func (a *IdentitiesAPI) retrieveCredentials(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := callerUserID(req)
	providerName := req.QueryParameter("provider")
	required := req.Request.URL.Query()["required_access"]
	logger.Info("credential retrieval requested", "user_id", userID, "provider", providerName,
		"required_access", required)

	record, envelope, found, err := a.repo.GetIdentity(req.Request.Context(), userID, providerName)
	if err != nil {
		logger.Error("failed to load identity", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to load identity")
		return
	}
	if !found {
		logger.Warn("no identity for the requested provider")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "no stored identity for this provider")
		return
	}
	if record.Status == extidentities.StatusRevoked {
		logger.Warn("identity is revoked", "id", record.ID)
		requesthelper.WriteError(req, resp, http.StatusConflict, "the stored credential was revoked by the provider; re-authorization is required")
		return
	}
	organizationID, err := a.organizationForIdentity(req.Request.Context(), record)
	if err != nil {
		logger.Warn("retrieve: cannot resolve the identity's organization", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	rec, provider, providerFound, err := a.resolveProvider(req, organizationID, record.Provider)
	if err != nil {
		logger.Error("failed to resolve provider for retrieval", "provider", record.Provider, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to resolve provider")
		return
	}
	if !providerFound {
		// The credential outlived its provider, so nothing can parse its
		// envelope. Not an internal failure — but deliberately NOT a 409
		// either: 409 means "the user must (re)authorize", and the tool loop
		// parks a call for minutes on it. Nobody can connect a provider that
		// is not registered, so this must fail fast and reach an admin.
		logger.Warn("stored credential references a provider that no longer exists", "provider", record.Provider)
		requesthelper.WriteError(req, resp, http.StatusFailedDependency,
			fmt.Sprintf("provider %q is no longer registered, so this credential cannot be used; re-register the provider or reconnect the account", record.Provider))
		return
	}
	// required_access accepts either provider-defined level names or raw
	// scopes; both reduce to the concrete scopes the grant must cover.
	requiredScopes, _ := extidentities.ResolveAccess(rec, required)
	if !extidentities.HasAccess(record.Access, requiredScopes) {
		logger.Warn("identity does not cover the required access",
			"granted", record.Access, "required", requiredScopes,
			"not_granted", missingAccess(record.Access, requiredScopes))
		requesthelper.WriteError(req, resp, http.StatusConflict, "the stored credential does not cover the required access")
		return
	}
	cred, err := provider.RetrieveIdentity(req.Request.Context(), envelope)
	if err != nil {
		logger.Error("failed to parse stored credential", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "stored credential is unreadable")
		return
	}
	cred.AccessLevel = record.AccessLevel
	if len(cred.Access) == 0 {
		cred.Access = record.Access
	}
	logger.Info("credential returned", "id", record.ID, "provider", record.Provider,
		"credential_chars", len(cred.Credentials), "expires_at", cred.ExpiresAt, "status", record.Status)
	requesthelper.WriteJSON(req, resp, http.StatusOK, cred)
}

// Outcomes of asking a provider to invalidate a credential. Logged, never
// fatal: see revokeAll.
const (
	revokeRevoked     = "revoked"
	revokeUnsupported = "unsupported"
	revokeFailed      = "failed"
)

// revocationTally counts the outcomes of a batch. Deleted is filled in by
// the caller, because what the platform forgets and what the provider was
// told are two different facts and an operator needs both.
type revocationTally struct {
	Revoked     int
	Unsupported int
	Failed      int
}

// revokeAll asks each provider to invalidate the credentials in records,
// BEFORE the platform deletes them. Every path that destroys an identity
// goes through here — a user disconnecting, an account being deleted, a
// provider being force-deleted — so "deleted" cannot come to mean "silently
// left live at the provider" in one of them.
//
// Best effort by design. Every failure is counted and logged, and the caller
// deletes regardless: if a provider outage could veto a delete, a user who
// wants their credential gone would be stuck with it — the worse of the two
// outcomes, since the local copy is the part this platform controls.
//
// Providers are resolved once each: a cascade over many users' identities
// otherwise rebuilds the same provider (an OpenSearch read plus a client
// secret decryption) for every record.
func (a *IdentitiesAPI) revokeAll(req *restful.Request, logger *logging.Logger, records []extidentities.Identity) revocationTally {
	var tally revocationTally
	providers := map[string]extidentities.Provider{}
	for _, record := range records {
		organizationID, err := a.organizationForIdentity(req.Request.Context(), record)
		if err != nil {
			logger.Warn("cannot revoke: organization unresolved", "provider", record.Provider, "err", err)
		}
		key := extidentities.ProviderDocID(organizationID, record.Provider)
		provider, resolved := providers[key]
		if !resolved {
			_, p, found, err := a.resolveProvider(req, organizationID, record.Provider)
			if err != nil || !found {
				logger.Warn("cannot revoke: provider unavailable", "provider", record.Provider, "found", found, "err", err)
			}
			provider = p
			providers[key] = p
		}
		if provider == nil {
			tally.Failed++
			continue
		}
		envelope, err := a.repo.OpenEnvelope(record)
		if err != nil {
			// Nothing can be revoked from an envelope nobody can open; it
			// must still be deletable.
			logger.Warn("identity could not be opened; nothing to revoke", "id", record.ID, "err", err)
			tally.Failed++
			continue
		}
		switch err := provider.Revoke(req.Request.Context(), envelope); {
		case err == nil:
			logger.Info("credential revoked at the provider", "provider", record.Provider, "id", record.ID)
			tally.Revoked++
		case errors.Is(err, extidentities.ErrRevocationUnsupported):
			// Not a failure: a PAT, or an OAuth record with no revocation
			// endpoint. The user must remove access at the provider themselves.
			logger.Info("provider offers no revocation; deleting the local copy only",
				"provider", record.Provider, "provider_type", record.ProviderType)
			tally.Unsupported++
		default:
			logger.Warn("revocation failed; deleting the local copy anyway",
				"provider", record.Provider, "err", err)
			tally.Failed++
		}
	}
	return tally
}

// revocationOutcome renders a single-identity tally as one log value.
func revocationOutcome(tally revocationTally) string {
	switch {
	case tally.Revoked > 0:
		return revokeRevoked
	case tally.Unsupported > 0:
		return revokeUnsupported
	default:
		return revokeFailed
	}
}

func (a *IdentitiesAPI) deleteIdentity(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := callerUserID(req)
	providerName := req.QueryParameter("provider")
	logger.Info("delete identity requested", "user_id", userID, "provider", providerName)

	record, found, err := a.repo.GetIdentityMetadata(req.Request.Context(), userID, providerName)
	if err != nil {
		logger.Error("failed to load identity", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete identity")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "no stored identity for this provider")
		return
	}
	revocation := revocationOutcome(a.revokeAll(req, logger, []extidentities.Identity{record}))
	if err := a.repo.DeleteIdentity(req.Request.Context(), userID, providerName); err != nil {
		logger.Error("failed to delete identity", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete identity")
		return
	}
	logger.Info("identity deleted", "user_id", userID, "provider", providerName, "revocation", revocation)
	resp.WriteHeader(http.StatusNoContent)
}

// deleteAllIdentities drops every credential one user holds, revoking each
// at its provider first. This is the account-deletion path: an account that
// is gone must not leave sealed credentials behind, still being refreshed by
// a sweep that selects on expiry rather than on whether the owner exists.
func (a *IdentitiesAPI) deleteAllIdentities(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	userID := callerUserID(req)
	logger.Info("delete all identities requested", "user_id", userID)
	if userID == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "no user to delete identities for")
		return
	}

	ctx := req.Request.Context()
	records, err := a.repo.ListIdentities(ctx, userID, "", "")
	if err != nil {
		logger.Error("failed to list the user's identities", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete identities")
		return
	}
	tally := a.revokeAll(req, logger, records)
	out := deletedIdentitiesResponse{
		Revoked:               tally.Revoked,
		RevocationUnsupported: tally.Unsupported,
		RevocationFailed:      tally.Failed,
	}
	deleted, err := a.repo.DeleteIdentitiesByUser(ctx, userID)
	if err != nil {
		logger.Error("failed to delete the user's identities", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete identities")
		return
	}
	out.Deleted = deleted
	logger.Info("identities deleted", "user_id", userID, "deleted", out.Deleted, "revoked", out.Revoked,
		"revocation_unsupported", out.RevocationUnsupported, "revocation_failed", out.RevocationFailed)
	requesthelper.WriteJSON(req, resp, http.StatusOK, out)
}
