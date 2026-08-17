package rbac

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"enact/internal/identity"
)

// Enforcer is how a service asks "may the caller do this?".
//
// Every service checks its own permissions rather than trusting a gateway to
// have checked — the downstream services are reachable by any signed service
// caller, so a check that lives only in enact-main is a check that a second
// caller can walk around. To make that affordable, a user's organization and
// rules are fetched once and cached for a short TTL, then evaluated locally:
// one round trip per user per window, not one per action.
//
// The cost is staleness. A role revoked at the service keeps working for up
// to the TTL. That is the deliberate trade; it is measured in seconds, and
// Forget exists for the paths that must not wait (a user removed from an
// organization, a role deleted).
type Enforcer struct {
	client *Client
	ttl    time.Duration

	mu      sync.RWMutex
	cached  map[string]cachedEffective
	members map[string]cachedMembers
}

type cachedEffective struct {
	effective Effective
	expires   time.Time
}

type cachedMembers struct {
	members []string
	expires time.Time
}

// NewEnforcer returns an Enforcer over the client, caching for cfg.EffectiveTTL.
func NewEnforcer(client *Client, cfg ClientConfig) *Enforcer {
	ttl := cfg.EffectiveTTL
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &Enforcer{
		client:  client,
		ttl:     ttl,
		cached:  map[string]cachedEffective{},
		members: map[string]cachedMembers{},
	}
}

// DeniedError is a refusal to perform an action. It names the permission,
// because "forbidden" with no subject is unactionable for whoever has to fix
// the role.
type DeniedError struct {
	UserID     string
	Permission string
	// NoOrganization distinguishes "you are in no organization yet" from
	// "your roles do not cover this" — the first is fixed by an
	// administrator approving a request, the second by an organization owner.
	NoOrganization bool
}

func (e *DeniedError) Error() string {
	if e.NoOrganization {
		return "you do not belong to an organization yet; request one and ask an administrator to approve it"
	}
	return fmt.Sprintf("permission denied: %s", e.Permission)
}

// Denied reports whether an error is an authorization refusal rather than a
// failure to reach a decision.
func Denied(err error) bool {
	var denied *DeniedError
	return errors.As(err, &denied)
}

// asDenied extracts a *DeniedError, for the HTTP mapping in http.go.
func asDenied(err error, target **DeniedError) bool {
	return errors.As(err, target)
}

// Effective resolves a user, from cache when it is fresh.
func (e *Enforcer) Effective(ctx context.Context, userID string) (Effective, error) {
	effective, _, err := e.effective(ctx, userID)
	return effective, err
}

// effective also reports whether the answer came from the cache, which the
// deny path uses to decide whether a refusal is worth re-checking.
func (e *Enforcer) effective(ctx context.Context, userID string) (Effective, bool, error) {
	now := time.Now()
	e.mu.RLock()
	entry, ok := e.cached[userID]
	e.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.effective, true, nil
	}
	effective, err := e.client.Effective(ctx, userID)
	if err != nil {
		return Effective{}, false, err
	}
	e.mu.Lock()
	e.cached[userID] = cachedEffective{effective: effective, expires: now.Add(e.ttl)}
	e.mu.Unlock()
	return effective, false, nil
}

// Require authorizes the caller in ctx for one permission. It returns a
// *DeniedError when the answer is no, and a transport error when the answer
// is unknown — a caller must not confuse the two, because an outage that
// reads as a denial locks everyone out, and one that reads as a grant lets
// everyone in.
func (e *Enforcer) Require(ctx context.Context, permission string) error {
	userID := identity.FromContext(ctx)
	effective, cached, err := e.effective(ctx, userID)
	if err != nil {
		return err
	}
	// A refusal decided from CACHED rules is re-checked once against the
	// service. Every service caches independently, so rules granted a moment
	// ago — most often the ownership of a resource the caller just created
	// elsewhere — are invisible here until the TTL expires. Without this, a
	// user creates an agent and cannot run it for the next few seconds.
	//
	// Only refusals pay the extra call, and only when they came from cache,
	// so the hot path of allowed requests is untouched. Rules are additive
	// within a window, so re-checking can only ever turn a no into a yes:
	// a revocation still takes effect at the end of the TTL, not sooner.
	if cached && (effective.OrganizationID == "" || !effective.Allows(permission)) {
		e.Forget(userID)
		if effective, _, err = e.effective(ctx, userID); err != nil {
			return err
		}
	}
	if effective.OrganizationID == "" {
		return &DeniedError{UserID: userID, Permission: permission, NoOrganization: true}
	}
	if !effective.Allows(permission) {
		return &DeniedError{UserID: userID, Permission: permission}
	}
	return nil
}

// RequireResource is Require for one action on one resource.
func (e *Enforcer) RequireResource(ctx context.Context, resource, action, id string) error {
	return e.Require(ctx, Permission(resource, action, id))
}

// Organization returns the caller's organization, or a *DeniedError when they
// have none. Services stamp it on resources they create and filter their
// queries by it.
func (e *Enforcer) Organization(ctx context.Context) (string, error) {
	userID := identity.FromContext(ctx)
	effective, err := e.Effective(ctx, userID)
	if err != nil {
		return "", err
	}
	if effective.OrganizationID == "" {
		return "", &DeniedError{UserID: userID, NoOrganization: true}
	}
	return effective.OrganizationID, nil
}

// CallerEffective resolves the caller in ctx, so a handler can filter a list
// locally instead of asking once per candidate.
func (e *Enforcer) CallerEffective(ctx context.Context) (Effective, error) {
	return e.Effective(ctx, identity.FromContext(ctx))
}

// OrganizationMembers lists the user ids in the caller's organization —
// which is how a listing finds candidates, since a resource's organization
// is its owner's and nothing else records it.
//
// Cached beside the effective rules and on the same TTL: a member added to
// the organization becomes visible to listings within one window.
func (e *Enforcer) OrganizationMembers(ctx context.Context) ([]string, error) {
	organizationID, err := e.Organization(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	e.mu.RLock()
	entry, ok := e.members[organizationID]
	e.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.members, nil
	}
	members, err := e.client.Members(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	e.mu.Lock()
	e.members[organizationID] = cachedMembers{members: ids, expires: now.Add(e.ttl)}
	e.mu.Unlock()
	return ids, nil
}

// Visible keeps the ids the caller may view, dropping the rest. The
// candidates come from the caller's own organization, so this decides what
// their ROLES reach: a member with no roles keeps only what they own, an
// owner keeps everything.
func (e Effective) Visible(resource string, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if e.Allows(Permission(resource, ActionView, id)) {
			out = append(out, id)
		}
	}
	return out
}

// RequireOrganization refuses a resource that belongs to another
// organization, and is the counterpart to Require rather than a substitute
// for it: Require answers "may you perform this action", this answers "is
// this yours to perform it on".
//
// Both are needed because owner bypass makes the first unconditional. An
// organization owner passes every permission check by construction, so
// without this an owner of one organization could read, edit and delete any
// resource of any other simply by naming its id. The bypass is meant to say
// "an owner may do anything INSIDE their organization"; this is the boundary
// that makes that true.
//
// The refusal is a *DeniedError so WriteDenied renders it as 404 alongside
// permission denials: a caller must not be able to tell "this exists but is
// not yours" from "this does not exist".
//
// An empty organizationID is refused too. A resource written before the
// organization was stored has none, and treating that as "belongs to
// everyone" would make a half-migrated cluster fail open.
func (e *Enforcer) RequireOrganization(ctx context.Context, organizationID string) error {
	userID := identity.FromContext(ctx)
	caller, err := e.Organization(ctx)
	if err != nil {
		return err
	}
	if organizationID == "" || organizationID != caller {
		return &DeniedError{UserID: userID, Permission: "organization:" + organizationID}
	}
	return nil
}

// Forget drops a user's cached decision. Call it immediately after their
// rules change — in particular after recording ownership of a resource they
// just created, or the cache would answer from rules resolved BEFORE the
// grant and the creator would be refused their own new resource until the
// TTL expired.
func (e *Enforcer) Forget(userID string) {
	e.mu.Lock()
	delete(e.cached, userID)
	e.mu.Unlock()
}
