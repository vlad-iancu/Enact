// Package enactworkflows implements workflow authoring and execution intake:
// CRUD over workflow definitions, and the endpoint that queues a run.
//
// It deliberately does NOT execute anything. Running a workflow can take
// minutes of model calls, so the work is handed to enact-workflow-runner over
// a Redis stream and this service stays a fast, ordinary API.
package enactworkflows

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/agents"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/queue"
	"enact/internal/rbac"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/workflows"
)

// Config wires the runtime, OpenSearch, the Redis stream that carries queued
// executions, the agent service (used to validate the agents a workflow's
// steps reference), and RBAC.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	Queue      queue.Config
	Workflows  workflows.Config
	AgentAPI   agents.ClientConfig
	RBAC       rbac.ClientConfig
	S2S        s2s.Config
}

func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		repo := workflows.NewRepository(osClient, cfg.Workflows)
		if err := repo.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify workflow index", "err", err)
			return nil, err
		}
		executions := workflows.NewExecutionRepository(osClient, cfg.Workflows)
		if err := executions.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify workflow execution index", "err", err)
			return nil, err
		}
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		agentClient := agents.NewClient(cfg.AgentAPI, s2sRuntime.Transport(nil, "enact-agent-management-api"))
		// Authorization is this service's own job: it is reachable by any
		// signed service caller, so a check that lives only in enact-main is a
		// check a second caller walks around.
		rbacClient := rbac.NewClient(cfg.RBAC, s2sRuntime.Transport(nil, "enact-rbac"))
		enforcer := rbac.NewEnforcer(rbacClient, cfg.RBAC)
		producer := queue.NewProducer(cfg.Queue)
		logger.Info("workflow api initialized", "stream", cfg.Queue.Stream,
			"agent_api", cfg.AgentAPI.BaseURL, "rbac", cfg.RBAC.BaseURL, "s2s_key_id", cfg.S2S.KeyID)

		ws := newWorkflowAPI(repo, executions, agentClient, producer, rbacClient, enforcer, logger).WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
