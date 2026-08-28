package workflows

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func shapesFixture(t *testing.T) Shapes {
	t.Helper()
	w := Workflow{
		ID:          "w1",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Steps: []Step{
			{Name: "classify", Type: StepTypeAgent, AgentID: "withSchema", Prompt: "hi"},
			{Name: "summarize", Type: StepTypeAgent, AgentID: "noSchema", Prompt: "hi"},
			{Name: "shape", Type: StepTypeCode, Code: "function run(c){return c;}",
				OutputSchema: json.RawMessage(`{"type":"object","properties":{"band":{"type":"string"}}}`)},
			{Name: "freeform", Type: StepTypeCode, Code: "function run(c){return c;}"},
		},
	}
	got, err := ResolveShapes(w, map[string]json.RawMessage{
		"withSchema": json.RawMessage(`{"type":"object","properties":{"sentiment":{"type":"string"}}}`),
	})
	if err != nil {
		t.Fatalf("ResolveShapes: %v", err)
	}
	return got
}

func TestResolveShapesSources(t *testing.T) {
	got := shapesFixture(t)
	if len(got.Steps) != 4 {
		t.Fatalf("resolved %d steps, want 4", len(got.Steps))
	}
	want := []string{ShapeSourceAgent, ShapeSourceText, ShapeSourceStep, ShapeSourceUnknown}
	for i, source := range want {
		if got.Steps[i].OutputSource != source {
			t.Errorf("step %q source = %q, want %q", got.Steps[i].Name, got.Steps[i].OutputSource, source)
		}
	}
	// An agent with no schema answers in prose, and that IS knowable — the
	// editor should be told it is a string, not left guessing.
	if string(got.Steps[1].OutputSchema) != `{"type":"string"}` {
		t.Errorf("a schemaless agent resolved to %s, want a string schema", got.Steps[1].OutputSchema)
	}
	// A code step that declares nothing has genuinely nothing to say.
	if len(got.Steps[3].OutputSchema) != 0 {
		t.Errorf("an undeclared code step resolved to %s, want nothing", got.Steps[3].OutputSchema)
	}
}

// The context schema is the endpoint's reason to exist: it must describe
// exactly what is addressable at that position, and nothing more.
func TestResolveShapesContextGrowsWithTheRun(t *testing.T) {
	got := shapesFixture(t)

	first := decode(t, got.Steps[0].ContextSchema)
	firstProps := first["properties"].(map[string]any)
	if _, has := firstProps["previous"]; has {
		t.Errorf("the first step's context declares `previous`; there is no previous step")
	}
	firstSteps := firstProps["steps"].(map[string]any)["properties"].(map[string]any)
	if len(firstSteps) != 0 {
		t.Errorf("the first step can address %v; it should be able to address nothing", firstSteps)
	}

	// By the third step, the two before it are addressable — and only those.
	third := decode(t, got.Steps[2].ContextSchema)
	thirdProps := third["properties"].(map[string]any)
	if _, has := thirdProps["previous"]; !has {
		t.Errorf("the third step's context does not declare `previous`")
	}
	thirdSteps := thirdProps["steps"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"classify", "summarize"} {
		if _, has := thirdSteps[name]; !has {
			t.Errorf("step 3 cannot address %q", name)
		}
	}
	if _, has := thirdSteps["shape"]; has {
		t.Errorf("step 3 can address itself")
	}
	if _, has := thirdSteps["freeform"]; has {
		t.Errorf("step 3 can address a step that has not run yet")
	}

	// The trigger input is reachable everywhere and is the workflow's own.
	if _, has := thirdProps["input"]; !has {
		t.Errorf("the context does not declare `input`")
	}
}

// A step whose output shape is unknown must not appear in the next step's
// `steps` — completing against a type nobody declared would be a fiction.
func TestResolveShapesOmitsUnknownOutputs(t *testing.T) {
	w := Workflow{ID: "w", Steps: []Step{
		{Name: "mystery", Type: StepTypeCode, Code: "function run(c){return c;}"},
		{Name: "after", Type: StepTypeCode, Code: "function run(c){return c;}"},
	}}
	got, err := ResolveShapes(w, nil)
	if err != nil {
		t.Fatalf("ResolveShapes: %v", err)
	}
	props := decode(t, got.Steps[1].ContextSchema)["properties"].(map[string]any)
	if _, has := props["previous"]; has {
		t.Errorf("`previous` is declared after a step with an unknown shape")
	}
	if steps := props["steps"].(map[string]any)["properties"].(map[string]any); len(steps) != 0 {
		t.Errorf("a step with an unknown shape appears as %v", steps)
	}
}

// Every context schema must itself be a usable JSON Schema — a client is
// going to compile it.
func TestResolveShapesProducesCompilableSchemas(t *testing.T) {
	got := shapesFixture(t)
	for _, step := range got.Steps {
		if _, err := CompileSchema(step.ContextSchema); err != nil {
			t.Errorf("step %q produced an uncompilable context schema: %v\n%s", step.Name, err, step.ContextSchema)
		}
	}
}

// A workflow with no input schema still produces a well-formed context: the
// trigger input becomes `true`, JSON Schema for "anything".
func TestResolveShapesWithoutAnInputSchema(t *testing.T) {
	w := Workflow{ID: "w", Steps: []Step{{Name: "only", Type: StepTypeCode, Code: "function run(c){return c;}"}}}
	got, err := ResolveShapes(w, nil)
	if err != nil {
		t.Fatalf("ResolveShapes: %v", err)
	}
	props := decode(t, got.Steps[0].ContextSchema)["properties"].(map[string]any)
	if props["input"] != true {
		t.Errorf("input = %v, want true (anything)", props["input"])
	}
	if _, err := CompileSchema(got.Steps[0].ContextSchema); err != nil {
		t.Errorf("uncompilable: %v", err)
	}
}
