package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

// Bounds on a code step. A code step is arbitrary JavaScript written by a
// user and run inside our process, so every one of these is load-bearing.
const (
	// MaxCodeBytes caps the source. Checked when a workflow is SAVED, so an
	// oversized step is an authoring error rather than a run-time surprise.
	MaxCodeBytes = 64 << 10 // 64 KiB
	// MaxCodeOutputBytes caps what a step may return. Without it one step
	// could write a document large enough to make every later read of the
	// execution expensive.
	MaxCodeOutputBytes = 1 << 20 // 1 MiB
	// DefaultCodeTimeout bounds wall-clock execution. Overridable per
	// deployment; see the runner's WORKFLOW_CODE_TIMEOUT.
	DefaultCodeTimeout = 5 * time.Second
)

// entrypoint is the function name a code step must define.
//
// A named function rather than a bare expression or an implicit return: it
// gives the author somewhere obvious to put helpers, and it makes the
// contract — one input, one return — visible in the code itself.
const entrypoint = "run"

// RunCode evaluates a code step against the step context and returns its
// output as JSON.
//
// The runtime is built fresh for every step and nothing is installed on it.
// goja provides only the ECMAScript standard library — no require, no
// filesystem, no network, no timers — so a script's entire reach is the
// object it is handed. That is the sandbox: not a policy that could be
// misconfigured, but an environment with nothing in it.
//
// What this does NOT bound is CPU: an interrupt stops a runaway script, but
// only after the timeout has elapsed, and it burns a core until then. That is
// why code steps run in the runner rather than in the API service.
func RunCode(source string, ctx Context, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = DefaultCodeTimeout
	}
	if len(source) > MaxCodeBytes {
		return nil, fmt.Errorf("code is %d bytes; the limit is %d", len(source), MaxCodeBytes)
	}

	vm := goja.New()
	// The script receives plain Go values decoded from JSON, so what it sees
	// matches what a template sees and what the record shows.
	payload, err := ctx.AsJSON()
	if err != nil {
		return nil, err
	}
	var input any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("build the step context: %w", err)
	}

	// Interrupt is the only thing standing between a `while (true) {}` and a
	// stuck worker. Armed before any user code is evaluated, including the
	// top-level definition, because an infinite loop can just as easily sit
	// outside the function.
	timer := time.AfterFunc(timeout, func() {
		vm.Interrupt(fmt.Sprintf("execution exceeded %s", timeout))
	})
	defer timer.Stop()

	if _, err := vm.RunString(source); err != nil {
		return nil, codeError("evaluate", err)
	}
	fn, ok := goja.AssertFunction(vm.Get(entrypoint))
	if !ok {
		return nil, fmt.Errorf("code must define a function named %q, for example: function %s(input) { return input; }",
			entrypoint, entrypoint)
	}
	value, err := fn(goja.Undefined(), vm.ToValue(input))
	if err != nil {
		return nil, codeError("run", err)
	}

	exported := value.Export()
	// undefined exports as nil, which marshals to null — a step that returns
	// nothing produces null rather than failing. Later steps then see null,
	// which is honest.
	out, err := json.Marshal(exported)
	if err != nil {
		return nil, fmt.Errorf("the returned value is not JSON-serialisable: %w", err)
	}
	if len(out) > MaxCodeOutputBytes {
		return nil, fmt.Errorf("returned %d bytes; the limit is %d", len(out), MaxCodeOutputBytes)
	}
	return out, nil
}

// codeError unwraps goja's error types into something a workflow author can
// act on: a thrown Error keeps its message and an interrupt says it timed out,
// rather than both arriving as an opaque wrapper.
func codeError(phase string, err error) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return fmt.Errorf("%v", interrupted.Value())
	}
	var exception *goja.Exception
	if errors.As(err, &exception) {
		return fmt.Errorf("%s: %v", phase, exception.Value())
	}
	return fmt.Errorf("%s: %w", phase, err)
}
