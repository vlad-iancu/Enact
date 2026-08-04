package enactmodelmanagement

import (
	"net/http"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/models"
	"enact/internal/requesthelper"
)

// ModelsAPI serves the available-models catalogue.
type ModelsAPI struct {
	logger *logging.Logger
}

func newModelsAPI(logger *logging.Logger) *ModelsAPI { return &ModelsAPI{logger: logger} }

type listModelsResponse struct {
	Models []models.Model `json:"models"`
}

func (a *ModelsAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1/models").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON)

	ws.Route(ws.GET("").
		To(a.list).
		Doc("List the models available to agents and inference").
		Returns(http.StatusOK, "OK", listModelsResponse{}))

	return ws
}

func (a *ModelsAPI) list(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("list models requested")
	available := models.List()
	logger.Info("models listed", "count", len(available))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listModelsResponse{Models: available})
}
