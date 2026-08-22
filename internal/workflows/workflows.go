// Package workflows holds the workflow domain: definitions and the record of
// every execution. It is a standalone package because two services share it —
// the workflow API owns authoring and intake, and the runner owns execution —
// and services do not import each other (ADR-0009).
//
// A workflow is deliberately a straight line: an ordered list of steps, each
// fed by what came before. Nothing here decides what runs next; the author
// does, at save time. That is what makes an execution reproducible and its
// stored record worth reading.
package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/opensearch"
)

// Config holds the OpenSearch index names for the workflow domain.
type Config struct {
	Index           string `env:"OPENSEARCH_INDEX_WORKFLOWS, default=enact-workflows"`
	ExecutionsIndex string `env:"OPENSEARCH_INDEX_WORKFLOW_EXECUTIONS, default=enact-workflow-executions"`
}

// Step types.
const (
	// StepTypeAgent runs an existing agent, with a templated prompt.
	StepTypeAgent = "agent"
	// StepTypeCode runs a JavaScript function over the step context. It is the
	// glue between agents: reshaping, filtering and arithmetic, which a model
	// should not be asked to do reliably.
	StepTypeCode = "code"
)

// Execution and step statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	// StatusSkipped marks a step that never ran because an earlier one failed.
	// Distinct from "failed" so a reader can tell the cause from the fallout.
	StatusSkipped = "skipped"
)

// Step is one unit of a workflow.
type Step struct {
	ID string `json:"id"`
	// Name addresses the step's output from later steps, as
	// {{ .Steps.<name> }}. Unique within a workflow, which is what makes that
	// reference unambiguous.
	Name string `json:"name"`
	Type string `json:"type"`

	// AgentID names the agent an agent step runs.
	AgentID string `json:"agent_id,omitempty"`
	// Prompt is the agent step's message, as a Go template over the step
	// context. Rendered per execution, never stored rendered — the template is
	// the definition, the rendering is part of the record.
	Prompt string `json:"prompt,omitempty"`

	// Code is a code step's JavaScript body. It receives the step context and
	// returns a JSON-serialisable value.
	Code string `json:"code,omitempty"`
}

// Workflow is an ordered list of steps.
type Workflow struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	// OrganizationID is the organization this workflow belongs to. Stored
	// rather than inferred: every read compares it, and an owner bypasses
	// permission checks, so this is what keeps one organization out of
	// another's data.
	OrganizationID string `json:"organization_id"`

	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Steps       []Step    `json:"steps"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// StepRun is what happened when one step ran.
//
// Input is stored alongside Output because a chain of model calls is
// untraceable without it: "step 3 failed" is not a diagnosis, "step 3 was
// given this and failed" is.
type StepRun struct {
	StepID string `json:"step_id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`

	Input  json.RawMessage `json:"input,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`

	// Prompt is an agent step's RENDERED prompt — the text actually sent. The
	// template lives on the workflow and may since have been edited, so
	// without this a past execution cannot be explained.
	Prompt string `json:"prompt,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Execution is one run of a workflow.
type Execution struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	// UserID is who triggered it, and therefore who every step acts as. Set by
	// the API from the authenticated caller and never taken from a request
	// body — it is the whole of the runner's authority.
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`

	Status string `json:"status"`
	// Input is the trigger payload, addressable as {{ .Input }}.
	Input json.RawMessage `json:"input,omitempty"`
	// Steps are the definitions as they were WHEN THIS RAN, copied onto the
	// record. A workflow can be edited afterwards; without the copy an old
	// execution would be read against a definition it never used.
	Steps []Step `json:"steps"`
	// Runs is what each step actually did, in order.
	Runs []StepRun `json:"runs"`
	// Output is the last step's output — the workflow's result.
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`

	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Repository persists workflow definitions.
type Repository struct {
	os    *opensearch.Client
	index string
}

func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{os: os, index: cfg.Index}
}

// EnsureIndex verifies the workflow index exists. The index and its mapping
// are owned by the composable template in mappings/ and created by
// `make infrastructure-up`; this fails fast when it is missing.
func (r *Repository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("workflows: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

func (r *Repository) Create(ctx context.Context, w Workflow) error {
	body, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, w.ID, body)
}

// Update replaces an existing workflow (full update).
func (r *Repository) Update(ctx context.Context, w Workflow) error { return r.Create(ctx, w) }

// Get fetches one workflow, scoped to an organization.
//
// A workflow belonging to a different organization is reported as ABSENT
// rather than refused: callers render that as 404, and "not yours" must be
// indistinguishable from "does not exist". The organization is a parameter
// rather than something the caller checks afterwards because an organization
// owner passes every permission check by construction — so one forgotten
// comparison would expose another organization's data.
func (r *Repository) Get(ctx context.Context, organizationID, id string) (Workflow, bool, error) {
	var w Workflow
	found, err := r.os.GetSource(ctx, r.index, id, &w)
	if err != nil || !found {
		return Workflow{}, found, err
	}
	if organizationID == "" || w.OrganizationID != organizationID {
		return Workflow{}, false, nil
	}
	return w, true, nil
}

// List returns an organization's workflows, newest first. The caller then
// drops what their rules do not cover.
func (r *Repository) List(ctx context.Context, organizationID string) ([]Workflow, error) {
	if organizationID == "" {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"organization_id": organizationID}},
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("workflows: marshal list query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]Workflow, 0, len(hits))
	for _, h := range hits {
		var w Workflow
		if err := json.Unmarshal(h.Source, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	return r.os.DeleteDoc(ctx, r.index, id)
}

// ExecutionRepository persists execution records.
type ExecutionRepository struct {
	os    *opensearch.Client
	index string
}

func NewExecutionRepository(os *opensearch.Client, cfg Config) *ExecutionRepository {
	return &ExecutionRepository{os: os, index: cfg.ExecutionsIndex}
}

func (r *ExecutionRepository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("workflows: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// Save writes an execution record, overwriting any previous version. The
// runner calls this after every step, so a caller polling mid-run sees
// progress rather than only the final state.
func (r *ExecutionRepository) Save(ctx context.Context, e Execution) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, e.ID, body)
}

// Get fetches one execution, scoped to an organization — same rule as
// Repository.Get, for the same reason.
func (r *ExecutionRepository) Get(ctx context.Context, organizationID, id string) (Execution, bool, error) {
	var e Execution
	found, err := r.os.GetSource(ctx, r.index, id, &e)
	if err != nil || !found {
		return Execution{}, found, err
	}
	if organizationID == "" || e.OrganizationID != organizationID {
		return Execution{}, false, nil
	}
	return e, true, nil
}

// GetUnscoped fetches an execution without an organization filter.
//
// For the RUNNER only, which acts on a message rather than on behalf of a
// caller and has no organization of its own to compare against. Every
// user-facing path must use Get: this one would happily return another
// organization's record.
func (r *ExecutionRepository) GetUnscoped(ctx context.Context, id string) (Execution, bool, error) {
	var e Execution
	found, err := r.os.GetSource(ctx, r.index, id, &e)
	if err != nil || !found {
		return Execution{}, found, err
	}
	return e, true, nil
}

// ListByWorkflow returns a workflow's executions, newest first.
func (r *ExecutionRepository) ListByWorkflow(ctx context.Context, organizationID, workflowID string, size int) ([]Execution, error) {
	if organizationID == "" || workflowID == "" {
		return nil, nil
	}
	if size <= 0 || size > 200 {
		size = 50
	}
	body, err := json.Marshal(map[string]any{
		"size": size,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"organization_id": organizationID}},
			map[string]any{"term": map[string]any{"workflow_id": workflowID}},
		}}},
		"sort": []any{map[string]any{"queued_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("workflows: marshal execution list query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]Execution, 0, len(hits))
	for _, h := range hits {
		var e Execution
		if err := json.Unmarshal(h.Source, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// DeleteByWorkflow removes a workflow's executions, for the delete cascade.
func (r *ExecutionRepository) DeleteByWorkflow(ctx context.Context, workflowID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"workflow_id": workflowID}},
	})
	if err != nil {
		return fmt.Errorf("workflows: marshal execution delete query: %w", err)
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
}
