package s2s

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/golang-jwt/jwt/v5"

	"enact/internal/requesthelper"
)

// clockLeeway tolerates small clock differences between services when
// validating exp/iat.
const clockLeeway = 5 * time.Second

// Filter is the S2S enforcement middleware. Register it on each business
// WebService (ws.Filter(runtime.Filter)) — at that level go-restful has
// already selected the route, so the ACL can match on the route template.
//
// Flow: authenticate the caller (missing token => "anonymous", invalid
// token => 401), then authorize caller+route against the ACL (no rule or
// unlisted caller => 403, default deny).
func (r *Runtime) Filter(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	logger := requesthelper.Logger(req, r.logger)

	caller, err := r.authenticate(req.Request)
	if err != nil {
		logger.Warn("s2s authentication failed", "err", err)
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, fmt.Sprintf("invalid service token: %v", err))
		return
	}

	// go-restful reports a WebService's root route as "<path>/" (trailing
	// slash); normalize so ACL keys are written without one.
	routePath := strings.TrimSuffix(req.SelectedRoutePath(), "/")
	route := req.Request.Method + " " + routePath
	if !r.acl.allowed(route, caller) {
		logger.Warn("s2s access denied", "caller", caller, "route", route)
		requesthelper.WriteError(req, resp, http.StatusForbidden, fmt.Sprintf("caller %q is not allowed on %q", caller, route))
		return
	}
	logger.Info("s2s authorized", "caller", caller, "route", route)
	chain.ProcessFilter(req, resp)
}

// authenticate resolves the caller identity of a request. No Authorization
// header means the anonymous caller; a present token must verify against
// the JWKS (signature, exp, aud = this service, iss consistent with kid).
func (r *Runtime) authenticate(hr *http.Request) (string, error) {
	header := hr.Header.Get("Authorization")
	if header == "" {
		return Anonymous, nil
	}
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", fmt.Errorf("authorization header is not a bearer token")
	}

	token, err := jwt.Parse(raw, r.keyFor,
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithAudience(r.self),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockLeeway),
	)
	if err != nil {
		return "", err
	}

	iss, err := token.Claims.GetIssuer()
	if err != nil || iss == "" {
		return "", fmt.Errorf("token has no issuer")
	}
	// The signing key and the claimed identity must be the same service, or
	// any key holder could impersonate any issuer.
	if kid, _ := token.Header["kid"].(string); kid != iss {
		return "", fmt.Errorf("token kid %q does not match issuer %q", token.Header["kid"], iss)
	}
	return iss, nil
}

// keyFor resolves the verification key for a token from its kid header.
func (r *Runtime) keyFor(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("token has no kid header")
	}
	key, ok := r.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	return key, nil
}