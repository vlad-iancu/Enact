package enactworkflowrunner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"enact/internal/extidentities"
	"enact/internal/files"
	"enact/internal/identity"
	"enact/internal/inference"
	"enact/internal/logging"
	"enact/internal/queue"
	"enact/internal/workflows"
)

// Runner executes queued workflow runs.
type Runner struct {
	executions *workflows.ExecutionRepository
	inference  *inference.Client
	// files holds the bytes an agent step attaches to its prompt, and the bytes
	// a Google Docs export produces. Nil when no store is configured, which
	// makes such a step fail with that as its reason rather than with a nil
	// dereference.
	files files.Store
	// identities resolves the credential a provider-backed step acts with, for
	// the user who triggered the run.
	identities *extidentities.Client
	// google calls Google's REST APIs. Its own client rather than the default:
	// an export is a large streamed body, and a timeout that covered it would
	// sever big documents partway through.
	google *http.Client
	// codeTimeout bounds one code step's wall clock.
	codeTimeout time.Duration
	// stepTimeout bounds one agent step. An agent with tools can legitimately
	// take minutes, so this is generous — but not unbounded, or one wedged
	// step would hold a run open forever.
	stepTimeout time.Duration
	logger      *logging.Logger
}

func newRunner(executions *workflows.ExecutionRepository, inferenceClient *inference.Client,
	fileStore files.Store, identitiesClient *extidentities.Client, googleClient *http.Client,
	codeTimeout, stepTimeout time.Duration, logger *logging.Logger) *Runner {
	if googleClient == nil {
		googleClient = &http.Client{}
	}
	return &Runner{
		executions:  executions,
		inference:   inferenceClient,
		files:       fileStore,
		identities:  identitiesClient,
		google:      googleClient,
		codeTimeout: codeTimeout,
		stepTimeout: stepTimeout,
		logger:      logger,
	}
}

// Handle runs one queued execution.
//
// The returned error decides the message's fate: nil acknowledges it, and
// anything else leaves it pending for the reclaim sweep. A FAILING WORKFLOW
// IS NOT AN ERROR HERE — a step that throws is a recorded outcome, not a
// delivery to retry. Only an inability to record the outcome is worth
// retrying, because that is the only case where trying again could help.
func (r *Runner) Handle(ctx context.Context, msg queue.ExecutionMessage) error {
	logger := r.logger.WithFields("execution_id", msg.ExecutionID)

	execution, found, err := r.executions.GetUnscoped(ctx, msg.ExecutionID)
	if err != nil {
		logger.Error("failed to load the execution", "err", err)
		return err
	}
	if !found {
		// The workflow was deleted, taking its executions with it. Nothing to
		// run and nothing to record; acknowledge rather than retry forever.
		logger.Warn("execution not found; dropping the message")
		return nil
	}
	if execution.Status != workflows.StatusQueued {
		// A redelivery of work that already started. Re-running would double
		// every model call and overwrite the record with a second attempt, so
		// it is refused rather than repeated.
		logger.Warn("execution is not queued; refusing to run it again", "status", execution.Status)
		return nil
	}

	logger = logger.WithFields("workflow_id", execution.WorkflowID, "user_id", execution.UserID)
	logger.Info("execution started", "steps", len(execution.Steps))

	execution.Status = workflows.StatusRunning
	execution.StartedAt = time.Now().UTC()
	if err := r.executions.Save(ctx, execution); err != nil {
		// Nothing has run yet, so retrying is safe and useful.
		logger.Error("failed to mark the execution running", "err", err)
		return err
	}

	// Every step acts as the user who triggered the run. This is the runner's
	// entire authority: it holds no permissions of its own, and the services
	// it calls re-check this user's rules — an agent step reaching
	// enact-model-inference is refused there if that user may not use the
	// agent.
	runCtx := identity.WithUserID(ctx, execution.UserID)

	execution.Runs = make([]workflows.StepRun, 0, len(execution.Steps))
	failed := false
	for _, step := range execution.Steps {
		if failed {
			// Recorded rather than omitted, so reading the record shows the
			// whole shape of the run and where it stopped.
			execution.Runs = append(execution.Runs, workflows.StepRun{
				StepID: step.ID, Name: step.Name, Type: step.Type, Status: workflows.StatusSkipped,
			})
			continue
		}
		run := r.runStep(runCtx, logger, step, execution)
		execution.Runs = append(execution.Runs, run)
		if run.Status == workflows.StatusFailed {
			failed = true
			execution.Error = fmt.Sprintf("step %q failed: %s", step.Name, run.Error)
		} else {
			execution.Output = run.Output
		}
		// Saved after every step so a client polling mid-run sees progress
		// rather than a black box that eventually turns green.
		if err := r.executions.Save(ctx, execution); err != nil {
			logger.Error("failed to record step progress", "step", step.Name, "err", err)
		}
	}

	execution.Status = workflows.StatusSucceeded
	if failed {
		execution.Status = workflows.StatusFailed
		execution.Output = nil
	}
	execution.FinishedAt = time.Now().UTC()
	if err := r.executions.Save(ctx, execution); err != nil {
		// The steps have already run; retrying would re-run them. Report and
		// acknowledge instead of doubling the work.
		logger.Error("failed to record the final state; not retrying", "err", err)
		return nil
	}
	logger.Info("execution finished", "status", execution.Status,
		"steps_run", len(execution.Runs), "duration_ms", time.Since(execution.StartedAt).Milliseconds())
	return nil
}

// runStep executes one step and returns its record. It never returns an
// error: a failure is part of the record.
func (r *Runner) runStep(ctx context.Context, logger *logging.Logger, step workflows.Step, execution workflows.Execution) workflows.StepRun {
	run := workflows.StepRun{
		StepID: step.ID, Name: step.Name, Type: step.Type,
		Status: workflows.StatusRunning, StartedAt: time.Now().UTC(),
	}
	fail := func(err error) workflows.StepRun {
		run.Status = workflows.StatusFailed
		run.Error = err.Error()
		run.FinishedAt = time.Now().UTC()
		logger.Warn("step failed", "step", step.Name, "type", step.Type, "err", err)
		return run
	}

	stepCtx, err := workflows.NewContext(execution.Input, execution.Runs)
	if err != nil {
		return fail(err)
	}
	// The context is recorded as the step's input: it is exactly what the step
	// was given, which is what makes a failure diagnosable after the fact.
	if encoded, err := stepCtx.AsJSON(); err == nil {
		run.Input = encoded
	}

	switch step.Type {
	case workflows.StepTypeAgent:
		prompt, err := workflows.RenderPrompt(step.Name, step.Prompt, stepCtx)
		if err != nil {
			return fail(err)
		}
		run.Prompt = prompt

		callCtx, cancel := context.WithTimeout(ctx, r.stepTimeout)
		defer cancel()

		// Resolved before the call rather than alongside it: an attachment
		// that cannot be read is an authoring mistake, and paying for a model
		// call to discover it helps nobody.
		attachments, err := r.attachmentsFor(callCtx, step, stepCtx)
		if err != nil {
			return fail(err)
		}
		// Recorded before the call, so a step that fails mid-inference still
		// shows what the model was given.
		run.Attachments = recorded(attachments)

		resp, err := r.inference.Invoke(callCtx, inference.Request{
			AgentID:      step.AgentID,
			Messages:     []inference.Message{{Role: "user", Content: prompt}},
			ContextFiles: sent(attachments),
		})
		if err != nil {
			if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
				return fail(fmt.Errorf("the agent did not answer within %s", r.stepTimeout))
			}
			return fail(err)
		}
		// A reply that parses as JSON is stored as JSON, so an agent with an
		// output_schema composes with the steps after it.
		run.Output = workflows.EncodeOutput(resp.Content)
		logger.Info("agent step completed", "step", step.Name, "agent_id", step.AgentID,
			"prompt_chars", len(prompt), "attachments", len(attachments), "output_bytes", len(run.Output),
			"input_tokens", resp.InputTokens, "output_tokens", resp.OutputTokens)

	case workflows.StepTypeCode:
		output, err := workflows.RunCode(step.Code, stepCtx, r.codeTimeout)
		if err != nil {
			return fail(err)
		}
		// A declared output schema is enforced, not merely advertised. An
		// unenforced schema drifts from what the code actually returns, and
		// then misleads every later step — and every editor completing
		// against it.
		if err := workflows.ValidateAgainst(step.OutputSchema, output); err != nil {
			return fail(fmt.Errorf("returned a value that does not match this step's output_schema: %w", err))
		}
		run.Output = output
		logger.Info("code step completed", "step", step.Name, "output_bytes", len(output),
			"schema_enforced", len(step.OutputSchema) > 0)

	default:
		if !workflows.IsGoogleStep(step.Type) {
			return fail(fmt.Errorf("unknown step type %q", step.Type))
		}
		callCtx, cancel := context.WithTimeout(ctx, r.stepTimeout)
		defer cancel()
		output, err := r.runGoogleStep(callCtx, logger, step, stepCtx, execution)
		if err != nil {
			if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
				return fail(fmt.Errorf("google did not answer within %s", r.stepTimeout))
			}
			return fail(err)
		}
		run.Output = output
		logger.Info("google step completed", "step", step.Name, "type", step.Type,
			"operation", step.Operation, "provider", step.Provider)
	}

	run.Status = workflows.StatusSucceeded
	run.FinishedAt = time.Now().UTC()
	return run
}
