package service

import (
	restful "github.com/emicklei/go-restful/v3"
	swgui "github.com/swaggest/swgui/v5"
)

// swaggerUIPrefix is the URL prefix where the embedded Swagger UI lives.
const swaggerUIPrefix = "/swagger-ui/"

// registerSwaggerUI mounts an embedded Swagger UI (assets compiled into the
// binary) at /swagger-ui/, pointed at the spec served at /api_docs.json. The
// supplied name is shown as the UI's page title.
func registerSwaggerUI(container *restful.Container, name string) {
	container.Handle(swaggerUIPrefix, swgui.NewHandler(name, "/api_docs.json", swaggerUIPrefix))
}
