package workflows

import (
	"fmt"
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
		return "", fmt.Errorf("workflows: render prompt of step %q: %w", name, err)
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
