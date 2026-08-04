package requesthelper

import (
	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
)

// Logger returns base bound to the request's context. The active span on
// that context gives every record a trace_id/span_id — both on the printed
// line and on the OTLP record shipped to Loki — which is what powers the
// log <-> trace links in Grafana.
//
// Handlers should derive their request-scoped logger through this instead
// of using their injected base logger directly:
//
//	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id)
func Logger(req *restful.Request, base *logging.Logger) *logging.Logger {
	return base.WithContext(req.Request.Context())
}