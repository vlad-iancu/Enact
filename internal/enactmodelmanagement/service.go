// Package enactmodelmanagement exposes the catalogue of models available to
// the platform. Each model has a friendly name mapped to an underlying
// Amazon Bedrock model id (see the models package).
package enactmodelmanagement

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/service"
)

// Config is the service configuration; model-management has no dependencies
// beyond the generic service runtime.
type Config struct {
	service.Config
}

// Build returns the service.Builder for the model-management API.
func Build(cfg *Config) service.Builder {
	return func(_ context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		logger.Info("model management api initialized")
		return []*restful.WebService{newModelsAPI(logger).WebService()}, nil
	}
}
