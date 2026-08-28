package workflows

import (
	"encoding/json"
	"fmt"
)

// Where a step's output shape came from, so a reader can tell a declared
// contract from a derived one — and knows where to go to change it.
const (
	// ShapeSourceStep is a code step's own output_schema.
	ShapeSourceStep = "step"
	// ShapeSourceAgent is the agent's output_schema, read from the agent
	// record. Changing it means editing the agent, not the workflow.
	ShapeSourceAgent = "agent"
	// ShapeSourceText is an agent with no output_schema: its reply is prose,
	// which a step receives as a JSON string. Knowable without being
	// declared anywhere.
	ShapeSourceText = "text"
	// ShapeSourceOperation is a step whose output shape follows from what it
	// does — a Google Docs export always produces a file descriptor — rather
	// than from anything anybody declared.
	ShapeSourceOperation = "operation"
	// ShapeSourceUnknown is a code step that declares nothing. JavaScript has
	// no declared return type, so there is genuinely nothing to say.
	ShapeSourceUnknown = "unknown"
)

// StepShape is one step's resolved input and output shapes.
type StepShape struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// AgentID is set for agent steps, so a client can follow the schema back
	// to the record that owns it.
	AgentID string `json:"agent_id,omitempty"`

	// OutputSchema is what this step produces, resolved. Absent when the
	// source is "unknown".
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	// OutputSource says where OutputSchema came from.
	OutputSource string `json:"output_source"`

	// ContextSchema describes the object this step RECEIVES — the shape of
	// {input, previous, steps} at this position. It is what an editor turns
	// into a type for `ctx`.
	//
	// Derived here rather than left to the caller because the rule for what
	// is addressable at a given step — everything before it, and nothing
	// else — is already implemented on this side, and a client
	// re-implementing it is a client that will eventually disagree.
	ContextSchema json.RawMessage `json:"context_schema"`
}

// Shapes is a workflow's resolved shapes, in step order.
type Shapes struct {
	WorkflowID string `json:"workflow_id"`
	// InputSchema is the workflow's own, repeated here so a client building
	// types needs one response rather than two.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Steps       []StepShape     `json:"steps"`
}

// textSchema is what an agent without an output_schema produces: prose,
// stored as a JSON string.
var textSchema = json.RawMessage(`{"type":"string"}`)

// ResolveShapes computes every step's input and output shape.
//
// agentSchemas maps an agent id to that agent's output_schema; an id that is
// absent from the map, or present with nothing, is an agent that answers in
// prose. The caller does the fetching — this stays a pure function so the
// resolution rules can be tested without a service.
func ResolveShapes(w Workflow, agentSchemas map[string]json.RawMessage) (Shapes, error) {
	out := Shapes{WorkflowID: w.ID, InputSchema: w.InputSchema, Steps: make([]StepShape, 0, len(w.Steps))}

	// Accumulated as we walk forward, mirroring exactly what NewContext builds
	// at run time.
	known := map[string]json.RawMessage{}
	var previous json.RawMessage

	for _, step := range w.Steps {
		contextSchema, err := contextSchemaFor(w.InputSchema, previous, known)
		if err != nil {
			return Shapes{}, err
		}
		shape := StepShape{
			Name: step.Name, Type: step.Type, AgentID: step.AgentID,
			ContextSchema: contextSchema,
		}

		switch step.Type {
		case StepTypeAgent:
			if schema := agentSchemas[step.AgentID]; len(schema) > 0 {
				shape.OutputSchema, shape.OutputSource = schema, ShapeSourceAgent
			} else {
				shape.OutputSchema, shape.OutputSource = textSchema, ShapeSourceText
			}
		case StepTypeCode:
			if len(step.OutputSchema) > 0 {
				shape.OutputSchema, shape.OutputSource = step.OutputSchema, ShapeSourceStep
			} else {
				shape.OutputSource = ShapeSourceUnknown
			}
		default:
			// Fixed by the operation: a Google step writes its own output, so
			// nothing here is the author's to declare or to get wrong.
			if schema := GoogleOutputSchema(step.Type, step.Operation); len(schema) > 0 {
				shape.OutputSchema, shape.OutputSource = schema, ShapeSourceOperation
			} else {
				shape.OutputSource = ShapeSourceUnknown
			}
		}
		out.Steps = append(out.Steps, shape)

		// Only a step with a known shape contributes to what the next one
		// sees. An unknown one is simply absent from `steps`, which is honest:
		// it can still be read at run time, there is just nothing to say about
		// it in advance.
		if len(shape.OutputSchema) > 0 {
			known[step.Name] = shape.OutputSchema
			previous = shape.OutputSchema
		} else {
			previous = nil
		}
	}
	return out, nil
}

// contextSchemaFor builds the JSON Schema of the object a step receives.
func contextSchemaFor(input, previous json.RawMessage, known map[string]json.RawMessage) (json.RawMessage, error) {
	steps := map[string]any{"type": "object", "properties": rawMap(known)}
	properties := map[string]any{
		"input": schemaOrAny(input),
		"steps": steps,
	}
	// `previous` is omitted entirely for the first step rather than being
	// declared and nullable: there is no previous step, and saying so by
	// absence is what makes an editor refuse to complete it.
	if len(previous) > 0 {
		properties["previous"] = json.RawMessage(previous)
	}
	body, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": properties,
	})
	if err != nil {
		return nil, fmt.Errorf("workflows: build context schema: %w", err)
	}
	return body, nil
}

// schemaOrAny renders an absent schema as `true` — the JSON Schema for
// "anything" — so the context schema is always well-formed rather than having
// a hole in it.
func schemaOrAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return true
	}
	return json.RawMessage(raw)
}

func rawMap(in map[string]json.RawMessage) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = json.RawMessage(v)
	}
	return out
}
