// Package enactmodelmanagement exposes the catalogue of models available to
// the platform. Each model has a friendly name mapped to an underlying
// Amazon Bedrock model id (see the models package).
package enactmodelmanagement

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config is the service configuration; model-management depends only on the
// generic service runtime and the platform S2S material.
type Config struct {
	service.Config
	S2S s2s.Config
}

// Build returns the service.Builder for the model-management API.
func Build(cfg *Config) service.Builder {
	return func(_ context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		logger.Info("model management api initialized", "s2s_key_id", cfg.S2S.KeyID)
		ws := newModelsAPI(logger).WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
