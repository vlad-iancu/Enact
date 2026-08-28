package workflows

import (
	"encoding/json"
	"strings"
	"testing"
)

// contextWith builds a step context from JSON, the way NewContext does from
// stored step output — so the tests walk the same decoded shapes a run does.
func contextWith(t *testing.T, input, previous string, steps map[string]string) Context {
	t.Helper()
	decode := func(raw string) any {
		if raw == "" {
			return nil
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return v
	}
	ctx := Context{Input: decode(input), Previous: decode(previous), Steps: map[string]any{}}
	for name, raw := range steps {
		ctx.Steps[name] = decode(raw)
	}
	return ctx
}

const reportFile = `{"$file":"fs:workflows/w1/executions/e9/files/2f1c","name":"q3.pdf","mime_type":"application/pdf","size":184320}`

func TestAttachmentResolvesEachRoot(t *testing.T) {
	ctx := contextWith(t,
		`{"upload":`+reportFile+`}`,
		`{"document":`+reportFile+`}`,
		map[string]string{"export": `{"files":[` + reportFile + `]}`},
	)

	for _, path := range []string{"input.upload", "previous.document", "steps.export.files.0"} {
		file, err := Attachment(ctx, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if file.Ref != "fs:workflows/w1/executions/e9/files/2f1c" || file.Name != "q3.pdf" {
			t.Errorf("%s resolved to %+v", path, file)
		}
		if file.Size != 184320 {
			t.Errorf("%s: size = %d, want 184320", path, file.Size)
		}
	}
}

// A path that resolves to the wrong thing must say so in terms the author can
// act on: the prompt would otherwise go to the model with nothing attached.
func TestAttachmentRefusesWhatIsNotAFile(t *testing.T) {
	ctx := contextWith(t,
		`{"n":5}`,
		`{"document":`+reportFile+`,"summary":"words","tags":["a"],"nested":{"deep":{}}}`,
		map[string]string{"classify": `{"label":"urgent"}`},
	)

	for _, tc := range []struct {
		what string
		path string
		says string
	}{
		{"missing field", "previous.missing", `there is no "missing"`},
		{"a string", "previous.summary", "not a file"},
		{"an object without the marker", "previous.nested.deep", "not a file"},
		{"a step that did not run", "steps.absent.file", `there is no "absent"`},
		{"indexing an object", "previous.document.0", `there is no "0"`},
		{"a field on a string", "previous.summary.name", "which has no"},
		{"an index past the end", "previous.tags.3", "no 3"},
		{"a non-numeric index", "previous.tags.first", `must be a number`},
		{"nothing to descend into", "input.n.deeper", "no \"deeper\""},
		{"a bad path", "$$$", "not a usable attachment path"},
		{"an unknown root", "outputs.file", "not a usable attachment path"},
		{"empty", "", "empty"},
	} {
		_, err := Attachment(ctx, tc.path)
		if err == nil {
			t.Errorf("%s: %q resolved cleanly, want a refusal", tc.what, tc.path)
			continue
		}
		if !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: error %q does not mention %q", tc.what, err, tc.says)
		}
	}
}

// The marker is the test, not the shape: an object that merely looks like a
// descriptor was not written by the store and must not be attachable.
func TestAttachmentRequiresTheFileMarker(t *testing.T) {
	ctx := contextWith(t, "", `{"lookalike":{"name":"q3.pdf","mime_type":"application/pdf","size":10}}`, nil)
	if _, err := Attachment(ctx, "previous.lookalike"); err == nil {
		t.Error("an object without $file was accepted as a file")
	}
}

func TestAttachmentRefusesAnEmptyReference(t *testing.T) {
	ctx := contextWith(t, "", `{"broken":{"$file":"","mime_type":"application/pdf"}}`, nil)
	if _, err := Attachment(ctx, "previous.broken"); err == nil {
		t.Error("a descriptor with no reference was accepted")
	}
}

func TestValidateAttachPathAcceptsWhatTheEditorOffers(t *testing.T) {
	for _, path := range []string{"input", "previous", "steps", "previous.document", "steps.export.files.0", "input.a_b.c9"} {
		if msg, ok := ValidateAttachPath(path); !ok {
			t.Errorf("%q was rejected: %s", path, msg)
		}
	}
	for _, path := range []string{"", "Previous.document", ".previous", "previous.", "previous..document", "context.previous", "previous.doc-ument"} {
		if _, ok := ValidateAttachPath(path); ok {
			t.Errorf("%q was accepted", path)
		}
	}
}

func TestValidateStepsChecksAttachments(t *testing.T) {
	agent := func(attach ...string) Step {
		return Step{Name: "summarise", Type: StepTypeAgent, AgentID: "a1", Prompt: "hi", Attach: attach}
	}

	if msg, ok := ValidateSteps([]Step{agent("previous.document")}); !ok {
		t.Errorf("a valid attachment was rejected: %s", msg)
	}

	// More than a model will take is a workflow that can never run, so it is
	// refused at save time rather than several minutes into a run.
	tooMany := agent("previous.a", "previous.b", "previous.c", "previous.d", "previous.e", "previous.f")
	if msg, ok := ValidateSteps([]Step{tooMany}); ok {
		t.Error("six attachments were accepted")
	} else if !strings.Contains(msg, "at most") {
		t.Errorf("message %q does not state the limit", msg)
	}

	if msg, ok := ValidateSteps([]Step{agent("context.previous")}); ok {
		t.Error("a malformed attachment path was accepted")
	} else if !strings.Contains(msg, "attachment path") {
		t.Errorf("message %q does not explain the path syntax", msg)
	}

	// A code step has no prompt to attach to, and could not read the bytes in
	// any case.
	code := Step{Name: "shape", Type: StepTypeCode, Code: "function run(c){return c}", Attach: []string{"previous.document"}}
	if msg, ok := ValidateSteps([]Step{code}); ok {
		t.Error("a code step was allowed to attach files")
	} else if !strings.Contains(msg, "agent step") {
		t.Errorf("message %q does not say which step type may attach", msg)
	}
}
