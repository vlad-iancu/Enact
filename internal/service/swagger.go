package service

import (
	restfulspec "github.com/emicklei/go-restful-openapi/v2"
	restful "github.com/emicklei/go-restful/v3"
	"github.com/go-openapi/spec"
)

// swaggerWebService builds a go-restful WebService that serves an OpenAPI 2.0
// (Swagger) spec at GET /api_docs.json describing every route registered on
// the given container. The supplied name is used as the spec's Info.Title.
// This WebService must be the last one added to the container — it captures
// the registered list at creation time.
func swaggerWebService(container *restful.Container, name string) *restful.WebService {
	cfg := restfulspec.Config{
		WebServices:                   container.RegisteredWebServices(),
		APIPath:                       "/api_docs.json",
		PostBuildSwaggerObjectHandler: makeSwaggerEnricher(name),
	}
	return restfulspec.NewOpenAPIService(cfg)
}

func makeSwaggerEnricher(name string) func(*spec.Swagger) {
	return func(s *spec.Swagger) {
		s.Info = &spec.Info{
			InfoProps: spec.InfoProps{
				Title:       name,
				Description: name + " API",
				Version:     "0.1.0",
			},
		}
		shortSummaries(s)
	}
}

// shortSummaries sets every operation's summary to its operationId — which
// go-restful derives from the Go handler function name (create, list,
// uploadDocument, ...) — and moves the original sentence from Doc() into the
// description. API clients that import the spec (e.g. Bruno) name requests
// after the summary, so this yields short names that match the code, while
// Swagger UI keeps the full text as the description.
func shortSummaries(s *spec.Swagger) {
	if s.Paths == nil {
		return
	}
	for _, item := range s.Paths.Paths {
		ops := []*spec.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch}
		for _, op := range ops {
			if op == nil || op.ID == "" {
				continue
			}
			if op.Description == "" {
				op.Description = op.Summary
			}
			op.Summary = op.ID
		}
	}
}
