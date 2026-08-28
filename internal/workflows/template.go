package workflows

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// maxPromptBytes bounds a rendered prompt. A template that interpolates a
// large upstream output can produce something far bigger than the template
// itself, and that is paid for on every turn of the agent's tool loop.
const maxPromptBytes = 256 << 10 // 256 KiB

// ParsePrompt checks a template without running it, so a typo is a save-time
// error rather than a failed execution ten minutes into a run.
func ParsePrompt(name, prompt string) error {
	_, err := newTemplate(name, prompt)
	return err
}

// RenderPrompt renders an agent step's prompt against the step context.
//
// text/template, not a bespoke syntax: it is what Go developers already know,
// it is in the standard library, and — unlike string substitution — it can
// express the ranges and conditionals a real prompt needs.
//
// It is text/template rather than html/template deliberately. The output is a
// prompt for a model, not markup for a browser, and HTML escaping would
// mangle quotes and angle brackets that a prompt legitimately contains.
func RenderPrompt(name, prompt string, ctx Context) (string, error) {
	tmpl, err := newTemplate(name, prompt)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, ctx); err != nil {
		return "", fmt.Errorf("workflows: render prompt of step %q: %w%s", name, err, explainRenderFailure(ctx))
	}
	if out.Len() > maxPromptBytes {
		return "", fmt.Errorf("workflows: step %q rendered a prompt of %d bytes; the limit is %d",
			name, out.Len(), maxPromptBytes)
	}
	return out.String(), nil
}

// newTemplate builds the template with the options every prompt shares.
func newTemplate(name, prompt string) (*template.Template, error) {
	// missingkey=error is the important one. The default renders a missing key
	// as "<no value>", which would send a prompt with a hole in it to a model
	// and get back a confident answer about nothing. A misspelled
	// {{ .Steps.clasify }} must stop the step.
	tmpl, err := template.New(name).Option("missingkey=error").Parse(prompt)
	if err != nil {
		return nil, fmt.Errorf("workflows: step %q has an invalid prompt template: %w", name, err)
	}
	return tmpl, nil
}

// explainRenderFailure adds what the template package cannot say.
//
// Its message for a field lookup on the wrong kind of value is
// "can't evaluate field doc_id in type interface {}", which names neither the
// value nor the mistake. By far the most common cause is a trigger sent as
// {"input": "{…}"} — the payload JSON-encoded a second time, so what should be
// an object arrives as a string. That is a one-character fix in the caller and
// an impenetrable error without this.
//
// It only ever ANNOTATES a failure that already happened, so it cannot reject
// anything legitimate: a workflow that genuinely wants a JSON-looking string
// as its input keeps working, and only sees this if a template asks that
// string for a field.
func explainRenderFailure(ctx Context) string {
	if hint := doubleEncoded("input", ctx.Input); hint != "" {
		return hint
	}
	// Steps before Previous, and in name order. Previous is always ALSO the
	// last successful step's output, so checking it first would report "the
	// previous step" for something that has a name — and a name is what the
	// author has to go and fix. Sorted so the message does not depend on map
	// iteration order.
	names := make([]string, 0, len(ctx.Steps))
	for name := range ctx.Steps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if hint := doubleEncoded("the output of step "+name, ctx.Steps[name]); hint != "" {
			return hint
		}
	}
	if hint := doubleEncoded("the previous step's output", ctx.Previous); hint != "" {
		return hint
	}
	return ""
}

// doubleEncoded reports a value that is a string whose contents are themselves
// a JSON object or array — the signature of one JSON encoding too many.
func doubleEncoded(what string, value any) string {
	text, isString := value.(string)
	if !isString {
		return ""
	}
	trimmed := strings.TrimSpace(text)
	if !looksLikeJSON(trimmed) || !json.Valid([]byte(trimmed)) {
		return ""
	}
	return fmt.Sprintf(
		` (%s is a JSON string containing JSON, not an object — it looks like it was sent as {"input": "{…}"} rather than {"input": {…}}, so its fields cannot be addressed)`,
		what)
}
