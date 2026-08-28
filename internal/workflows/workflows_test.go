package workflows

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func runs(pairs ...[2]string) []StepRun {
	out := make([]StepRun, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, StepRun{Name: p[0], Status: StatusSucceeded, Output: json.RawMessage(p[1])})
	}
	return out
}

// The whole point of the context is that step three can reach step one
// without step two having carried the value forward.
func TestContextExposesInputPreviousAndEarlierSteps(t *testing.T) {
	ctx, err := NewContext(
		json.RawMessage(`{"customer_id":"c-42"}`),
		runs([2]string{"classify", `{"label":"billing"}`}, [2]string{"draft", `{"body":"hello"}`}),
	)
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}

	got, err := RenderPrompt("reply",
		`{{ .Input.customer_id }}|{{ .Steps.classify.label }}|{{ .Previous.body }}`, ctx)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if want := "c-42|billing|hello"; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

// Previous must track the most recent SUCCESSFUL step, and a failed step must
// not appear at all — a template referring to it should fail rather than
// quietly render nothing into a prompt.
func TestContextOmitsFailedSteps(t *testing.T) {
	ctx, err := NewContext(nil, []StepRun{
		{Name: "ok", Status: StatusSucceeded, Output: json.RawMessage(`{"v":1}`)},
		{Name: "broken", Status: StatusFailed, Error: "boom"},
	})
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	if _, present := ctx.Steps["broken"]; present {
		t.Errorf("a failed step appears in the context")
	}
	if _, err := RenderPrompt("x", `{{ .Steps.broken.v }}`, ctx); err == nil {
		t.Errorf("referencing a failed step rendered without error")
	}
}

// A misspelled reference must stop the step. The default template behaviour
// renders "<no value>", which would send a prompt with a hole in it to a
// model and get back a confident answer about nothing.
func TestRenderPromptFailsOnMissingKey(t *testing.T) {
	ctx, err := NewContext(json.RawMessage(`{"a":1}`), runs([2]string{"classify", `{"label":"x"}`}))
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	out, err := RenderPrompt("s", `{{ .Steps.clasify.label }}`, ctx)
	if err == nil {
		t.Fatalf("a misspelled step reference rendered %q instead of failing", out)
	}
	if strings.Contains(out, "<no value>") {
		t.Errorf("rendered a placeholder into the prompt: %q", out)
	}
}

func TestParsePromptRejectsBrokenTemplates(t *testing.T) {
	if err := ParsePrompt("s", `{{ .Input.a `); err == nil {
		t.Errorf("an unclosed action parsed without error")
	}
	if err := ParsePrompt("s", `plain text with no actions`); err != nil {
		t.Errorf("a plain prompt failed to parse: %v", err)
	}
}

// An agent whose output_schema makes it answer in JSON should compose with
// the steps after it: the next step addresses fields, not a string it has to
// parse again.
func TestEncodeOutputKeepsJSONAsJSON(t *testing.T) {
	out := EncodeOutput(`  {"sentiment":"positive","score":0.9}  `)
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("JSON reply was not stored as JSON: %v (%s)", err, out)
	}
	if decoded["sentiment"] != "positive" {
		t.Errorf("decoded %v", decoded)
	}
}

func TestEncodeOutputKeepsProseAsString(t *testing.T) {
	out := EncodeOutput("Oslo is the capital.")
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("prose was not stored as a JSON string: %v (%s)", err, out)
	}
	if s != "Oslo is the capital." {
		t.Errorf("got %q", s)
	}
	// A bare number is an answer, not a document.
	if got := EncodeOutput("42"); string(got) != `"42"` {
		t.Errorf(`EncodeOutput("42") = %s, want a JSON string`, got)
	}
}

func TestRunCodeReturnsJSON(t *testing.T) {
	ctx, err := NewContext(json.RawMessage(`{"n":3}`), runs([2]string{"prior", `{"m":4}`}))
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	out, err := RunCode(`function run(input) {
		return { total: input.input.n + input.steps.prior.m, seen: Object.keys(input) };
	}`, ctx, time.Second)
	if err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	var decoded struct {
		Total int      `json:"total"`
		Seen  []string `json:"seen"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if decoded.Total != 7 {
		t.Errorf("total = %d, want 7", decoded.Total)
	}
	if len(decoded.Seen) != 3 {
		t.Errorf("the script saw %v, want input/previous/steps", decoded.Seen)
	}
}

// The interrupt is the only thing between a runaway script and a stuck
// worker.
func TestRunCodeInterruptsInfiniteLoops(t *testing.T) {
	start := time.Now()
	_, err := RunCode(`function run(input) { while (true) {} }`, Context{}, 200*time.Millisecond)
	if err == nil {
		t.Fatalf("an infinite loop returned without error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("interrupt took %s; the timeout is not being enforced", elapsed)
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error %q does not explain that it timed out", err)
	}
}

// A loop OUTSIDE the function is just as effective at hanging a worker, so
// the interrupt must be armed before any user code is evaluated.
func TestRunCodeInterruptsTopLevelLoops(t *testing.T) {
	_, err := RunCode(`while (true) {}
	function run(input) { return 1; }`, Context{}, 200*time.Millisecond)
	if err == nil {
		t.Fatalf("a top-level infinite loop returned without error")
	}
}

// The sandbox is an environment with nothing in it, not a policy. If any of
// these ever resolve, a code step has reached outside the process.
func TestRunCodeHasNoHostAccess(t *testing.T) {
	for _, global := range []string{"require", "process", "fetch", "XMLHttpRequest", "setTimeout", "eval_file"} {
		out, err := RunCode(`function run(i) { return typeof `+global+`; }`, Context{}, time.Second)
		if err != nil {
			t.Fatalf("probing %s: %v", global, err)
		}
		if string(out) != `"undefined"` {
			t.Errorf("%s is defined inside a code step (%s); the sandbox leaks", global, out)
		}
	}
}

func TestRunCodeRequiresAnEntrypoint(t *testing.T) {
	_, err := RunCode(`var x = 1;`, Context{}, time.Second)
	if err == nil {
		t.Fatalf("code without a run function was accepted")
	}
	if !strings.Contains(err.Error(), "run") {
		t.Errorf("error %q does not name the expected function", err)
	}
}

func TestRunCodeSurfacesThrownErrors(t *testing.T) {
	_, err := RunCode(`function run(i) { throw new Error("nope"); }`, Context{}, time.Second)
	if err == nil {
		t.Fatalf("a thrown error was swallowed")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q loses the thrown message", err)
	}
}

func TestRunCodeRejectsOversizedSource(t *testing.T) {
	_, err := RunCode(strings.Repeat("//x\n", MaxCodeBytes), Context{}, time.Second)
	if err == nil {
		t.Fatalf("oversized source was accepted")
	}
}

func TestValidateSteps(t *testing.T) {
	cases := []struct {
		name  string
		steps []Step
		ok    bool
		says  string
	}{
		{"empty", nil, false, "at least one step"},
		{"good", []Step{
			{Name: "classify", Type: StepTypeAgent, AgentID: "a1", Prompt: "{{ .Input }}"},
			{Name: "shape", Type: StepTypeCode, Code: "function run(i){return i;}"},
		}, true, ""},
		{"duplicate names", []Step{
			{Name: "a", Type: StepTypeCode, Code: "x"},
			{Name: "a", Type: StepTypeCode, Code: "x"},
		}, false, "unique"},
		{"unaddressable name", []Step{
			{Name: "my step", Type: StepTypeCode, Code: "x"},
		}, false, "step name"},
		{"unknown type", []Step{{Name: "a", Type: "magic"}}, false, "expected"},
		{"agent without agent id", []Step{
			{Name: "a", Type: StepTypeAgent, Prompt: "hi"},
		}, false, "names no agent"},
		{"broken template", []Step{
			{Name: "a", Type: StepTypeAgent, AgentID: "a1", Prompt: "{{ .Input "},
		}, false, "template"},
		{"code without code", []Step{{Name: "a", Type: StepTypeCode}}, false, "no code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := ValidateSteps(tc.steps)
			if ok != tc.ok {
				t.Fatalf("ok = %v (%s), want %v", ok, msg, tc.ok)
			}
			if !ok && !strings.Contains(msg, tc.says) {
				t.Errorf("message %q does not mention %q", msg, tc.says)
			}
		})
	}
}

func TestAgentIDsAreDistinct(t *testing.T) {
	got := AgentIDs([]Step{
		{Type: StepTypeAgent, AgentID: "a1"},
		{Type: StepTypeCode},
		{Type: StepTypeAgent, AgentID: "a1"},
		{Type: StepTypeAgent, AgentID: "a2"},
	})
	if len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
		t.Errorf("AgentIDs = %v, want [a1 a2]", got)
	}
}

// Marking a function async is a reflex for anyone who writes JavaScript.
// Without handling, it silently destroys the result: an async function
// returns a Promise, which marshals to {} — the step "succeeds" and the next
// one is handed nothing.
func TestRunCodeResolvesAnAsyncEntrypoint(t *testing.T) {
	out, err := RunCode(`async function run(ctx) { return { ok: true, n: 7 }; }`, Context{}, time.Second)
	if err != nil {
		t.Fatalf("async run: %v", err)
	}
	var decoded struct {
		OK bool `json:"ok"`
		N  int  `json:"n"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if !decoded.OK || decoded.N != 7 {
		t.Errorf("async run produced %s, want the returned object", out)
	}
}

func TestRunCodeResolvesAReturnedPromise(t *testing.T) {
	out, err := RunCode(`function run(ctx) { return Promise.resolve({ v: 1 }); }`, Context{}, time.Second)
	if err != nil {
		t.Fatalf("promise run: %v", err)
	}
	if string(out) != `{"v":1}` {
		t.Errorf("out = %s, want {\"v\":1}", out)
	}
}

// A rejected promise is a failure, exactly as throwing is.
func TestRunCodeFailsOnARejectedPromise(t *testing.T) {
	_, err := RunCode(`async function run(ctx) { throw new Error("nope"); }`, Context{}, time.Second)
	if err == nil {
		t.Fatalf("a rejected promise was treated as success")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q loses the rejection reason", err)
	}
}

// Nothing can settle a pending promise here — there is no event loop — so
// saying so beats returning an empty object or hanging until the interrupt.
func TestRunCodeExplainsAPendingPromise(t *testing.T) {
	_, err := RunCode(`function run(ctx) { return new Promise(function () {}); }`, Context{}, time.Second)
	if err == nil {
		t.Fatalf("a pending promise was accepted")
	}
	if !strings.Contains(err.Error(), "event loop") {
		t.Errorf("error %q does not explain why it can never settle", err)
	}
}

func TestParseCodeRejectsSyntaxErrors(t *testing.T) {
	err := ParseCode("broken", "function run(ctx) { return {")
	if err == nil {
		t.Fatalf("unbalanced braces parsed cleanly")
	}
	// The position is what lets an editor place a marker rather than just
	// showing a sentence.
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error %q does not name the step", err)
	}
	if !strings.Contains(err.Error(), "1:") && !strings.Contains(err.Error(), "line") {
		t.Errorf("error %q carries no position: %s", err, err.Error())
	}
	if err := ParseCode("fine", "function run(ctx) { return ctx.input; }"); err != nil {
		t.Errorf("valid code was rejected: %v", err)
	}
	// Valid syntax that will fail at run time is NOT a parse error: this
	// checks the code parses, not that it behaves.
	if err := ParseCode("later", "function notRun(ctx) { return 1; }"); err != nil {
		t.Errorf("a missing entrypoint should not be a syntax error: %v", err)
	}
}

// The template package's own message for this is "can't evaluate field doc_id
// in type interface {}", which names neither the value nor the mistake. The
// cause is almost always one JSON encoding too many in the caller.
func TestRenderPromptExplainsADoubleEncodedInput(t *testing.T) {
	ctx, err := NewContext(json.RawMessage(`"{\"doc_id\":\"1MLC\"}"`), nil)
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	_, err = RenderPrompt("step_docs", `{{ .Input.doc_id }}`, ctx)
	if err == nil {
		t.Fatalf("addressing a field on a string rendered without error")
	}
	for _, want := range []string{"JSON string containing JSON", `{"input": {…}}`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A step whose OUTPUT was double-encoded is the same mistake one hop later.
func TestRenderPromptExplainsADoubleEncodedStepOutput(t *testing.T) {
	ctx, err := NewContext(nil, runs([2]string{"classify", `"{\"label\":\"billing\"}"`}))
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	if _, err = RenderPrompt("reply", `{{ .Steps.classify.label }}`, ctx); err == nil {
		t.Fatalf("addressing a field on a string rendered without error")
	} else if !strings.Contains(err.Error(), "output of step classify") {
		t.Errorf("error %q does not name the step whose output is a string", err)
	}
}

// A workflow whose input is legitimately a string keeps working; the hint only
// ever annotates a failure that already happened.
func TestRenderPromptLeavesLegitimateStringsAlone(t *testing.T) {
	ctx, err := NewContext(json.RawMessage(`"{\"a\":1}"`), nil)
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	got, err := RenderPrompt("s", `the payload was {{ .Input }}`, ctx)
	if err != nil {
		t.Fatalf("rendering a string input failed: %v", err)
	}
	if !strings.Contains(got, `{"a":1}`) {
		t.Errorf("rendered %q, want the string itself", got)
	}
}
