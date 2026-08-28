// Package enactworkflowrunner implements the workflow execution worker. It
// consumes queued executions and runs their steps in order, updating the
// execution record as it goes.
//
// It is a separate process from the workflow API for two reasons. A run can
// take minutes of model calls, which has no business occupying an API host;
// and code steps execute user-authored JavaScript, which burns CPU here rather
// than where people's requests are served.
//
// The worker reuses the standard service runtime so it gets a /healthz probe
// and graceful shutdown; the consumer loop runs in a background goroutine
// started by Build and tied to the process lifetime.
package enactworkflowrunner

import (
	"context"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/extidentities"
	"enact/internal/files"
	"enact/internal/inference"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/queue"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/workflows"
)

// Config wires the runtime, OpenSearch, the execution stream, and the
// inference service that agent steps call.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	Queue      queue.Config
	Workflows  workflows.Config
	Inference  inference.ClientConfig
	Identities extidentities.ClientConfig
	Files      files.Config
	S2S        s2s.Config

	// CodeTimeout bounds one code step's wall clock. The only thing standing
	// between a `while (true) {}` and a wedged worker.
	CodeTimeout time.Duration `env:"WORKFLOW_CODE_TIMEOUT, default=5s"`
	// StepTimeout bounds one agent step. Generous, because an agent running
	// tools legitimately takes minutes — but finite, so one stuck step cannot
	// hold a run open forever.
	StepTimeout time.Duration `env:"WORKFLOW_STEP_TIMEOUT, default=10m"`
}

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
		executions := workflows.NewExecutionRepository(osClient, cfg.Workflows)
		if err := executions.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify workflow execution index", "err", err)
			return nil, err
		}
		inferenceClient := inference.NewClient(cfg.Inference, s2sRuntime.Transport(nil, "enact-model-inference"))
		fileStore, err := files.NewFS(cfg.Files)
		if err != nil {
			logger.Error("failed to open the workflow file store", "err", err)
			return nil, err
		}
		// Said out loud because three services must agree on it, and an
		// unconfigured root silently becomes a temp directory the OS may prune.
		logger.Info("workflow file store opened", "root", fileStore.Root(), "configured", cfg.Files.Root != "")
		// Provider-backed steps act as the person who triggered the run, so the
		// credential is fetched per step from the identities service rather than
		// held anywhere here.
		identitiesClient := extidentities.NewClient(cfg.Identities, s2sRuntime.Transport(nil, "enact-external-identities"))
		runner := newRunner(executions, inferenceClient, fileStore, identitiesClient, nil,
			cfg.CodeTimeout, cfg.StepTimeout, logger)

		consumer := queue.NewConsumer(cfg.Queue)
		consumer.Dropped = func(id string, deliveries int64) {
			logger.Warn("dropping poison execution message after max delivery attempts",
				"message_id", id, "deliveries", deliveries, "stream", cfg.Queue.Stream)
		}
		go func() {
			logger.Info("consuming workflow executions", "stream", cfg.Queue.Stream, "group", cfg.Queue.Group,
				"code_timeout", cfg.CodeTimeout, "step_timeout", cfg.StepTimeout)
			if err := consumer.RunExecutions(ctx, runner.Handle); err != nil {
				logger.Error("consumer stopped", "stream", cfg.Queue.Stream, "err", err)
			}
		}()

		return []*restful.WebService{}, nil
	}
}
