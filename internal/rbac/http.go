package rbac

import (
	"net/http"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/requesthelper"
)

// WriteDenied turns an authorization failure into a reply, identically in
// every service — one mapping, so two services cannot disagree about what a
// refusal looks like.
//
// The status depends on what the refusal reveals:
//
//   - A caller who belongs to no organization gets **403** with the reason.
//     There is no resource to hide, and the fix is an administrator
//     approving their organization request.
//   - A caller denied one named resource gets **404**, matching how MCP
//     servers and conversations already behave: a resource in another
//     organization is not "forbidden to you", it is none of your business
//     that it exists.
//   - Anything else is not a denial at all — it is the RBAC service being
//     unreachable, which must read as **502**, never as a refusal. An outage
//     that looks like a denial locks the platform out; one that looks like a
//     grant opens it.
//
// notFound is the message used for the 404, so each caller can name the
// thing the way its own routes do.
func WriteDenied(req *restful.Request, resp *restful.Response, err error, notFound string) {
	var denied *DeniedError
	if !asDenied(err, &denied) {
		requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to check permissions")
		return
	}
	if denied.NoOrganization {
		requesthelper.WriteError(req, resp, http.StatusForbidden, denied.Error())
		return
	}
	requesthelper.WriteError(req, resp, http.StatusNotFound, notFound)
}

// WriteDeniedForbidden is WriteDenied for actions with no resource to hide —
// creating something, or listing. There is nothing whose existence a 404
// would protect, so a refusal says so plainly.
func WriteDeniedForbidden(req *restful.Request, resp *restful.Response, err error) {
	var denied *DeniedError
	if !asDenied(err, &denied) {
		requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to check permissions")
		return
	}
	requesthelper.WriteError(req, resp, http.StatusForbidden, denied.Error())
}
