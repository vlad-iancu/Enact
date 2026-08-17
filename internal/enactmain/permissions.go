package enactmain

import (
	"encoding/json"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/rbac"
)

// resourceFlags tells the UI what the caller may do with one resource, so it
// can render a read-only row rather than offering controls that would be
// refused.
//
// They are HINTS, not the gate. Every one of these actions is authorized
// again by the service that performs it, and a stale flag only means a
// button appears that then refuses. What the flags do guarantee is
// agreement: they are computed with the same matcher the services enforce
// with, so a true here and a 403 there cannot both be right at the same
// instant.
type resourceFlags struct {
	Editable  bool `json:"editable"`
	Deletable bool `json:"deletable"`
	// Usable is the action that is not "manage the record": running an
	// agent, retrieving from a knowledge base, calling an MCP server's
	// tools, connecting an identity through a provider. A viewer typically
	// has view without use, which is exactly the case the UI needs to render
	// differently.
	Usable bool `json:"usable"`
}

// flagsFor decides the three flags for one resource id.
func flagsFor(effective rbac.Effective, resource, id string) resourceFlags {
	return resourceFlags{
		Editable:  effective.Allows(rbac.Permission(resource, rbac.ActionEdit, id)),
		Deletable: effective.Allows(rbac.Permission(resource, rbac.ActionDelete, id)),
		Usable:    effective.Allows(rbac.Permission(resource, rbac.ActionUse, id)),
	}
}

// effectiveFor resolves the caller's rules once for a request, so a listing
// costs one authorization lookup rather than one per row.
//
// A failure degrades to no rules rather than failing the response: the
// resources are the answer, and a listing with no edit buttons is a better
// outcome than no listing at all. The flags understate access when this
// happens, never overstate it.
func (a *MainAPI) effectiveFor(req *restful.Request, logger *logging.Logger, userID string) rbac.Effective {
	effective, err := a.rbac.Effective(req.Request.Context(), userID)
	if err != nil {
		logger.Warn("failed to resolve permissions; returning without action hints", "err", err)
		return rbac.Effective{}
	}
	return effective
}

// withFlags merges the flags into a JSON object this service is passing
// through verbatim. Used where enact-main relays a downstream response body
// it does not model — decoding into a map keeps every field the service
// sent, including ones added later, instead of pinning the shape here.
//
// On any decoding failure the original bytes are returned unchanged: a
// response missing its hints is still a usable response.
func withFlags(body []byte, flags resourceFlags) []byte {
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return body
	}
	object["editable"] = flags.Editable
	object["deletable"] = flags.Deletable
	object["usable"] = flags.Usable
	merged, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return merged
}
