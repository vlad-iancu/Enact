package workflows

import (
	"encoding/json"
	"fmt"
)

// Context is what a step can see: the trigger payload, the immediately
// preceding output, and every earlier step's output by name.
//
// All three are offered because a straight pipeline is not enough in practice:
// a value needed by step five would otherwise have to be copied through steps
// two, three and four, and each of those copies is a chance to lose it.
//
// The same object serves both step types — templates address it by field
// (.Input, .Previous, .Steps), code steps receive it as JSON (input,
// previous, steps). One shape, so moving logic between an agent and a code
// step does not mean rewriting how it reads its input.
type Context struct {
	Input    any            `json:"input"`
	Previous any            `json:"previous"`
	Steps    map[string]any `json:"steps"`
}

// NewContext builds the context for the next step from what has run so far.
//
// Values are decoded rather than carried as raw JSON: a template reaching
// {{ .Steps.classify.label }}, and a script reading steps.classify.label,
// both need real maps, not strings that happen to contain JSON.
func NewContext(input json.RawMessage, runs []StepRun) (Context, error) {
	ctx := Context{Steps: map[string]any{}}

	decoded, err := decodeJSON(input)
	if err != nil {
		return Context{}, fmt.Errorf("workflows: decode trigger input: %w", err)
	}
	ctx.Input = decoded

	for _, run := range runs {
		// Only completed steps contribute. A failed step's output is absent
		// rather than null, so a template referencing it fails loudly instead
		// of quietly rendering nothing into a prompt.
		if run.Status != StatusSucceeded {
			continue
		}
		value, err := decodeJSON(run.Output)
		if err != nil {
			return Context{}, fmt.Errorf("workflows: decode output of step %q: %w", run.Name, err)
		}
		ctx.Steps[run.Name] = value
		ctx.Previous = value
	}
	return ctx, nil
}

// decodeJSON turns stored raw JSON into template- and script-addressable
// values. Absent input decodes to nil rather than an error: a workflow
// triggered with no payload is ordinary.
func decodeJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// AsJSON renders the context as the object a code step receives.
func (c Context) AsJSON() (json.RawMessage, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("workflows: marshal step context: %w", err)
	}
	return body, nil
}

// EncodeOutput turns an agent's reply into a step output.
//
// A reply that parses as JSON is stored as JSON, so an agent with an
// output_schema composes with the steps after it — {{ .Previous.sentiment }}
// addresses a field rather than a string that has to be parsed again. Anything
// else is stored as a JSON string, which is what prose is.
func EncodeOutput(text string) json.RawMessage {
	trimmed := trimSpace(text)
	if looksLikeJSON(trimmed) && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	// Marshalling a string cannot fail.
	quoted, _ := json.Marshal(text)
	return quoted
}

// looksLikeJSON is a cheap gate before the real check, so ordinary prose is
// not run through a JSON validator. Only objects and arrays qualify: a bare
// number or the word "true" is far more likely to be an answer than a
// document, and treating it as one would surprise the next step.
func looksLikeJSON(s string) bool {
	if s == "" {
		return false
	}
	return s[0] == '{' || s[0] == '['
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
