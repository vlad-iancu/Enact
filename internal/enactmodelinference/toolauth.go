package enactmodelinference

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"enact/internal/extidentities"
	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/requesthelper"
	"enact/internal/tools"
)

// Reasons a required credential is not usable, as the UI sees them.
const (
	// reasonNotConnected: the user has never connected this provider.
	reasonNotConnected = "not_connected"
	// reasonInsufficientAccess: a credential exists but was revoked, or its
	// grant does not cover the required access level.
	reasonInsufficientAccess = "insufficient_access"
	// reasonUnavailable: the identity service could not answer.
	reasonUnavailable = "unavailable"
)

// missingRequirement is one unmet credential.
type missingRequirement struct {
	Provider    string `json:"provider"`
	AccessLevel string `json:"access_level,omitempty"`
	Reason      string `json:"reason"`
}

// toolCallAuthorizationRequiredEvent reports that a tool call could not run
// because the user has not connected everything it needs. It is TERMINAL:
// the call has already failed by the time it is emitted, and a
// toolCallResult marked as an error follows it.
//
// It carries COORDINATES, not a URL: this service does not know enact-main's
// public origin, so the frontend builds
// /identities/connect?provider=…&access_level=… for OAuth providers, or
// posts /identities/pat for token ones.
type toolCallAuthorizationRequiredEvent struct {
	ServerID  string               `json:"server_id"`
	Tool      string               `json:"tool"`
	ToolUseID string               `json:"tool_use_id"`
	Missing   []missingRequirement `json:"missing,omitempty"`
}

// toolAuthorizer resolves the CALLING user's credentials for a tool call and
// renders them into headers and params.
//
// It does NOT wait for missing credentials. A call whose requirements are
// unmet fails immediately, saying what to connect: parking an inference for
// minutes holds a model turn, an SSE stream and a Bedrock context open on the
// chance that somebody completes an OAuth flow in another tab.
//
// Deliberately the caller's credentials, not the agent owner's: a tool acts
// as the person who asked for it. (Knowledge-base loading differs on
// purpose — see applyAgent, which impersonates the agent owner because the
// KBs are the agent's configuration, not the caller's data.)
type toolAuthorizer struct {
	identities *extidentities.Client
	logger     *logging.Logger
}

// resolved is the outcome of resolving one tool's credentials.
type resolved struct {
	Credentials tools.Credentials
	Missing     []missingRequirement
	// Fatal is set when the failure is not the user's to fix (the identity
	// service is unreachable, or a requirement is malformed).
	Fatal error
}

// resolve fetches every credential a tool declares, for the calling user.
func (z *toolAuthorizer) resolve(ctx context.Context, logger *logging.Logger, server tools.Server, toolName string) resolved {
	reqs := server.Requirements(toolName)
	if len(reqs) == 0 {
		return resolved{}
	}
	out := resolved{Credentials: make(tools.Credentials, len(reqs))}

	for _, req := range reqs {
		cred, found, err := z.identities.Credentials(ctx, req.Provider, req.RequiredAccess())
		switch {
		case err == nil && found:
			out.Credentials[req.Provider] = cred
		case err == nil && !found:
			out.Missing = append(out.Missing, missingRequirement{
				Provider: req.Provider, AccessLevel: req.AccessLevel, Reason: reasonNotConnected,
			})
		default:
			// Only a conflict means "the user must (re)authorize"; a bad
			// request or an outage must not park the call for minutes.
			var conflict *extidentities.ConflictError
			if errors.As(err, &conflict) {
				logger.Warn("credential does not satisfy the requirement",
					"provider", req.Provider, "access_level", req.AccessLevel, "reason", conflict.Message)
				out.Missing = append(out.Missing, missingRequirement{
					Provider: req.Provider, AccessLevel: req.AccessLevel, Reason: reasonInsufficientAccess,
				})
				continue
			}
			// The provider was deregistered under a stored credential.
			// Waiting is pointless — the user has nothing left to connect —
			// so this fails the call and names the misconfiguration.
			var gone *extidentities.ProviderGoneError
			if errors.As(err, &gone) {
				logger.Error("tool requires a provider that is no longer registered",
					"provider", req.Provider, "server", server.ID, "tool", toolName)
				out.Fatal = fmt.Errorf("provider %q is no longer registered on the platform; an administrator must re-register it", req.Provider)
				return out
			}
			var badReq *requesthelper.BadRequestError
			if errors.As(err, &badReq) {
				out.Fatal = fmt.Errorf("credential lookup for %q rejected: %s", req.Provider, badReq.Message)
				return out
			}
			out.Fatal = fmt.Errorf("credential lookup for %q failed", req.Provider)
			return out
		}
	}
	logger.Info("tool credentials resolved", "tool", toolName,
		"required", len(reqs), "resolved", len(out.Credentials), "missing", len(out.Missing))
	return out
}

// probeHeaders renders the credentials a SERVER'S OWNER configured for the
// platform's own handshake (tools.ProbeInitialize). A server that
// authenticates its handshake refuses the session before any tool is named,
// so this is the key to the door; the caller's own credentials, resolved
// separately, decide who they are once inside.
//
// Deliberately the owner's, not the caller's: the same credential is used at
// registration and on the background refresh, where no caller exists. It
// means an agent can reach a server on the owner's access — which is what
// registering a gated server on someone's behalf means.
func (z *toolAuthorizer) probeHeaders(ctx context.Context, logger *logging.Logger, server tools.Server) (map[string]string, error) {
	reqs, auth, configured := server.Probe(tools.ProbeInitialize)
	if !configured {
		return nil, nil
	}
	ownerCtx := identity.WithUserID(ctx, server.Owner)
	creds := make(tools.Credentials, len(reqs))
	for _, req := range reqs {
		cred, found, err := z.identities.Credentials(ownerCtx, req.Provider, req.RequiredAccess())
		switch {
		case err != nil:
			return nil, fmt.Errorf("the server's owner has no usable %q credential", req.Provider)
		case !found:
			return nil, fmt.Errorf("the server's owner has not connected %q, which this server requires to open a session", req.Provider)
		}
		creds[req.Provider] = cred
	}
	headers, _, err := tools.Render(auth, creds)
	if err != nil {
		return nil, fmt.Errorf("the server's %s authorization could not be rendered: %w", tools.ProbeInitialize, err)
	}
	logger.Info("session probe credentials resolved", "server_id", server.ID, "owner", server.Owner,
		"headers", tools.HeaderNames(headers))
	return headers, nil
}

// missingMessage is what the model is told when a tool cannot run for lack
// of a credential. It names what to connect so the assistant can relay it,
// and says "then try again" because nothing will retry on the user's behalf.
func missingMessage(missing []missingRequirement) string {
	parts := make([]string, 0, len(missing))
	for _, m := range missing {
		part := fmt.Sprintf("%q", m.Provider)
		if m.AccessLevel != "" {
			part += fmt.Sprintf(" (access level %q)", m.AccessLevel)
		}
		if m.Reason == reasonInsufficientAccess {
			part += " needs re-authorization"
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("authorization required: connect %s, then try again",
		strings.Join(parts, " and "))
}
