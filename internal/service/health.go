package service

import (
	"net/http"

	restful "github.com/emicklei/go-restful/v3"
)

// healthPath is the liveness probe's root, shared with the duplicate-root
// check in service.go.
const healthPath = "/healthz"

// healthWebService exposes a liveness probe at GET /healthz. Every binary
// that calls service.Run gets this endpoint for free; service-specific
// Builders should not register their own.
func healthWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path(healthPath).Produces(restful.MIME_JSON)
	ws.Route(ws.GET("").
		To(handleHealth).
		Doc("Liveness probe").
		Returns(http.StatusOK, "OK", nil))
	return ws
}

func handleHealth(_ *restful.Request, resp *restful.Response) {
	resp.AddHeader("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	_, _ = resp.Write([]byte(`{"status":"ok"}`))
}
