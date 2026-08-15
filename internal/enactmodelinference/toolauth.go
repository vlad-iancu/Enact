package enactmodelinference

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// reasonUnavailable: the identity service could not answer. Waiting
	// cannot fix this, so the call fails instead of parking.
	reasonUnavailable = "unavailable"
)

// Statuses of the toolCallWaitingAuthorization event.
const (
	waitStatusWaiting  = "waiting"
	waitStatusResolved = "resolved"
	waitStatusTimeout  = "timeout"
)

// missingRequirement is one unmet credential.
type missingRequirement struct {
	Provider    string `json:"provider"`
	AccessLevel string `json:"access_level,omitempty"`
	Reason      string `json:"reason"`
}

// toolCallWaitingAuthorizationEvent announces that a tool call cannot
// proceed until the user connects an account, and later that it can.
//
// It carries COORDINATES, not a URL: this service does not know
// enact-main's public origin, so the frontend builds
// /identities/connect?provider=…&access_level=… for OAuth providers, or
// posts /identities/pat for token ones.
type toolCallWaitingAuthorizationEvent struct {
	ServerID    string               `json:"server_id"`
	Tool        string               `json:"tool"`
	ToolUseID   string               `json:"tool_use_id"`
	Status      string               `json:"status"`
	Missing     []missingRequirement `json:"missing,omitempty"`
	WaitSeconds int                  `json:"wait_timeout_seconds,omitempty"`
}

// toolAuthorizer resolves the CALLING user's credentials for a tool call,
// renders them into headers and params, and parks the call until they
// exist.
//
// Deliberately the caller's credentials, not the agent owner's: a tool acts
// as the person who asked for it. (Knowledge-base loading differs on
// purpose — see applyAgent, which impersonates the agent owner because the
// KBs are the agent's configuration, not the caller's data.)
type toolAuthorizer struct {
	identities *extidentities.Client
	waiters    *authWaiters
	recheck    time.Duration
	heartbeat  time.Duration
	waitFor    time.Duration
	logger     *logging.Logger
}

// resolved is the outcome of resolving one tool's credentials.
type resolved struct {
	Credentials tools.Credentials
	Missing     []missingRequirement
	// Fatal is set when waiting cannot help (the identity service is
	// unreachable, or a requirement is malformed).
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

// waitForAuthorization parks until every missing credential exists or the
// deadline passes. It returns the freshly resolved credentials.
//
// The wait happens entirely BEFORE the MCP call, so the tool client's
// CallTimeout never bounds it.
func (z *toolAuthorizer) waitForAuthorization(ctx context.Context, logger *logging.Logger, server tools.Server, toolName, toolUseID string, first resolved, deadline time.Time, emit func(string, any) error) (resolved, error) {
	userID := identity.FromContext(ctx)

	keys := make([]string, 0, len(server.Requirements(toolName)))
	for _, req := range server.Requirements(toolName) {
		keys = append(keys, waiterKey(userID, req.Provider))
	}
	// Register BEFORE re-checking: a credential stored between the check
	// and the registration would otherwise never wake this call.
	woken, release := z.waiters.Register(keys)
	defer release()

	current := first
	if current = z.resolve(ctx, logger, server, toolName); len(current.Missing) == 0 || current.Fatal != nil {
		return current, current.Fatal
	}

	if err := emit("toolCallWaitingAuthorization", toolCallWaitingAuthorizationEvent{
		ServerID: server.ID, Tool: toolName, ToolUseID: toolUseID,
		Status: waitStatusWaiting, Missing: current.Missing,
		WaitSeconds: int(time.Until(deadline).Seconds()),
	}); err != nil {
		return current, err
	}
	logger.Info("tool call waiting for authorization", "tool", toolName,
		"missing", len(current.Missing), "deadline", deadline, "pending_waiters", z.waiters.Pending())

	recheck := time.NewTicker(z.recheck)
	defer recheck.Stop()
	heartbeat := time.NewTicker(z.heartbeat)
	defer heartbeat.Stop()
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			// The browser hung up; nothing left to wait for.
			return current, ctx.Err()
		case <-timeout.C:
			logger.Warn("tool call authorization timed out", "tool", toolName)
			_ = emit("toolCallWaitingAuthorization", toolCallWaitingAuthorizationEvent{
				ServerID: server.ID, Tool: toolName, ToolUseID: toolUseID,
				Status: waitStatusTimeout, Missing: current.Missing,
			})
			return current, nil
		case <-heartbeat.C:
			// Re-announce: keeps the SSE connection warm through proxies and
			// lets a UI that connected late learn what to ask for.
			_ = emit("toolCallWaitingAuthorization", toolCallWaitingAuthorizationEvent{
				ServerID: server.ID, Tool: toolName, ToolUseID: toolUseID,
				Status: waitStatusWaiting, Missing: current.Missing,
				WaitSeconds: int(time.Until(deadline).Seconds()),
			})
		case <-woken:
			logger.Info("identity event woke a waiting tool call", "tool", toolName)
		case <-recheck.C:
		}

		if current = z.resolve(ctx, logger, server, toolName); current.Fatal != nil {
			return current, current.Fatal
		}
		if len(current.Missing) == 0 {
			logger.Info("tool call authorization satisfied", "tool", toolName)
			_ = emit("toolCallWaitingAuthorization", toolCallWaitingAuthorizationEvent{
				ServerID: server.ID, Tool: toolName, ToolUseID: toolUseID,
				Status: waitStatusResolved,
			})
			return current, nil
		}
	}
}

// missingMessage is what the model is told when a tool cannot run for lack
// of a credential. It names what to connect so the assistant can relay it.
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
