// Package identity centralises how the platform determines the calling user.
//
// Authentication and authorization are intentionally NOT implemented yet: any
// request may impersonate any user by setting the X-User-Id header. When the
// header is absent, a shared "default" user is assumed so the services are
// usable out of the box.
//
// The user id travels on the request context, mirroring how trace context
// flows: Filter (installed container-wide by the service runtime) resolves
// the impersonation header once per request and stores the id on the context;
// everything downstream — handlers, repositories, outbound service clients —
// reads it back with FromContext instead of taking a user id parameter.
package identity

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"
)

// Header is the request header used to impersonate a user.
const Header = "X-User-Id"

// DefaultUser is assumed when no impersonation header is supplied.
const DefaultUser = "default"

// ctxKey is the private context key for the user id; a private type
// guarantees no other package can collide with (or forge) the entry.
type ctxKey struct{}

// WithUserID returns a copy of ctx that carries userID.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, userID)
}

// FromContext returns the user id carried by ctx, or DefaultUser when the
// context has passed through no Filter (e.g. in tests or background work).
func FromContext(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKey{}).(string); ok && id != "" {
		return id
	}
	return DefaultUser
}

// Filter is a go-restful filter that resolves the calling user from the
// impersonation header and enriches the request context with it. The service
// runtime registers it container-wide, so every handler can rely on
// FromContext working.
func Filter(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	r := req.Request
	userID := r.Header.Get(Header)
	if userID == "" {
		userID = DefaultUser
	}
	req.Request = r.WithContext(WithUserID(r.Context(), userID))
	chain.ProcessFilter(req, resp)
}
