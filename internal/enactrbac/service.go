// Package enactrbac implements the RBAC service: organizations, the requests
// to create them, memberships, roles and the rules they grant. It is the
// source of truth every other service asks "may this user do this?" — through
// internal/rbac's client, never by reaching into this package.
package enactrbac

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/rbac"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config wires the runtime, OpenSearch and S2S.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	RBAC       rbac.Config
	S2S        s2s.Config
}

// Build constructs the RBAC service.
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		repo := rbac.NewRepository(osClient, cfg.RBAC)
		if err := repo.EnsureIndices(ctx); err != nil {
			logger.Error("failed to verify rbac indices", "err", err)
			return nil, err
		}

		api := &RBACAPI{repo: repo, logger: logger}
		logger.Info("rbac service initialized",
			"organizations_index", cfg.RBAC.OrganizationsIndex,
			"requests_index", cfg.RBAC.RequestsIndex,
			"memberships_index", cfg.RBAC.MembershipsIndex,
			"roles_index", cfg.RBAC.RolesIndex,
			"s2s_key_id", cfg.S2S.KeyID,
		)

		services := api.WebServices()
		if s2sRuntime.Enabled() {
			for _, ws := range services {
				ws.Filter(s2sRuntime.Filter)
			}
		}
		return services, nil
	}
}
