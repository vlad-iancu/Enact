package enactmain

import (
	"net/http"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/requesthelper"
)

// modelOption is what the frontend needs to render a model picker: the id
// to send on messages and the name to show people.
type modelOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type listModelsResponse struct {
	Models []modelOption `json:"models"`
}

// modelsWebService returns the session-guarded model catalogue route.
func (a *MainAPI) modelsWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/models").Produces(restful.MIME_JSON)
	ws.Filter(a.requireSession)

	ws.Route(ws.GET("").
		To(a.listModels).
		Doc("List the models available for conversations: id (for messages' model field) and display name (for the picker)").
		Returns(http.StatusOK, "OK", listModelsResponse{}))

	return ws
}

func (a *MainAPI) listModels(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list models requested")

	catalogue, err := a.models.List(req.Request.Context())
	if err != nil {
		logger.Error("failed to fetch model catalogue", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to fetch models")
		return
	}
	out := make([]modelOption, 0, len(catalogue))
	for _, m := range catalogue {
		display := m.DisplayName
		if display == "" {
			display = m.Name
		}
		out = append(out, modelOption{ID: m.Name, DisplayName: display})
	}
	logger.Info("models listed", "count", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listModelsResponse{Models: out})
}
