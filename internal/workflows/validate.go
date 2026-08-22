package workflows

import (
	"fmt"
	"regexp"
)

// MaxSteps bounds a workflow's length.
//
// Each agent step runs its own tool loop, so the model calls a workflow can
// make is steps × turns. This is the only ceiling on what one execution can
// cost, which is why it exists before anyone asks for it.
const MaxSteps = 25

// stepName is what a step may be called. Restricted because the name is a
// template path — {{ .Steps.classify }} — and template field syntax cannot
// address a name containing a space, a dash or a dot. Rejecting those at save
// time is far kinder than a template that mysteriously will not parse.
var stepName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateSteps checks everything about a workflow's steps that can be known
// without running them.
//
// It returns a message written for the person authoring the workflow, and
// callers surface it as a 400. Everything here fails at save time rather than
// mid-execution, which for a workflow matters more than usual: a run may not
// reach step seven for several minutes and several model calls.
//
// What it deliberately does NOT check is whether a referenced agent exists —
// that needs a call to another service, so it belongs to the caller that has
// one (see the workflow API's validate).
func ValidateSteps(steps []Step) (string, bool) {
	if len(steps) == 0 {
		return "a workflow needs at least one step", false
	}
	if len(steps) > MaxSteps {
		return fmt.Sprintf("a workflow may have at most %d steps; this one has %d", MaxSteps, len(steps)), false
	}

	seen := make(map[string]bool, len(steps))
	for i, step := range steps {
		position := fmt.Sprintf("step %d", i+1)
		if step.Name == "" {
			return fmt.Sprintf("%s has no name; names are how later steps refer to its output", position), false
		}
		if !stepName.MatchString(step.Name) {
			return fmt.Sprintf("%s is named %q; a step name must start with a letter or underscore and contain only letters, digits and underscores, because it is used as {{ .Steps.%s }}",
				position, step.Name, step.Name), false
		}
		if seen[step.Name] {
			return fmt.Sprintf("two steps are named %q; names must be unique so {{ .Steps.%s }} is unambiguous", step.Name, step.Name), false
		}
		seen[step.Name] = true

		switch step.Type {
		case StepTypeAgent:
			if step.AgentID == "" {
				return fmt.Sprintf("%s (%s) is an agent step but names no agent", position, step.Name), false
			}
			if step.Code != "" {
				return fmt.Sprintf("%s (%s) is an agent step but also carries code", position, step.Name), false
			}
			if step.Prompt == "" {
				return fmt.Sprintf("%s (%s) has no prompt", position, step.Name), false
			}
			if err := ParsePrompt(step.Name, step.Prompt); err != nil {
				return err.Error(), false
			}
		case StepTypeCode:
			if step.Code == "" {
				return fmt.Sprintf("%s (%s) is a code step but has no code", position, step.Name), false
			}
			if len(step.Code) > MaxCodeBytes {
				return fmt.Sprintf("%s (%s) has %d bytes of code; the limit is %d",
					position, step.Name, len(step.Code), MaxCodeBytes), false
			}
			if step.AgentID != "" || step.Prompt != "" {
				return fmt.Sprintf("%s (%s) is a code step but also names an agent or a prompt", position, step.Name), false
			}
		default:
			return fmt.Sprintf("%s (%s) has type %q; expected %q or %q",
				position, step.Name, step.Type, StepTypeAgent, StepTypeCode), false
		}
	}
	return "", true
}

// AgentIDs returns the distinct agents a workflow's steps reference, for the
// caller that validates them against the agent service.
func AgentIDs(steps []Step) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.Type != StepTypeAgent || step.AgentID == "" || seen[step.AgentID] {
			continue
		}
		seen[step.AgentID] = true
		out = append(out, step.AgentID)
	}
	return out
}
