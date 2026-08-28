package workflows

import (
	"encoding/json"
	"strings"
	"testing"
)

func docsStep(mutate func(*Step)) Step {
	step := Step{
		Name: "brief", Type: StepTypeGoogleDocs, Provider: "google",
		Operation: DocsOperationExport, DocumentID: "{{ .Input.doc_id }}", Format: "pdf",
	}
	if mutate != nil {
		mutate(&step)
	}
	return step
}

func TestValidateGoogleDocsStep(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Step)
		ok     bool
		says   string
	}{
		{"valid export", nil, true, ""},
		{"valid create", func(s *Step) {
			s.Operation, s.DocumentID, s.Format = DocsOperationCreate, "", ""
			s.Title, s.Body = "Summary of {{ .Input.doc_id }}", "{{ .Previous.text }}"
		}, true, ""},
		{"default format", func(s *Step) { s.Format = "" }, true, ""},

		{"no provider", func(s *Step) { s.Provider = "" }, false, "provider"},
		{"no operation", func(s *Step) { s.Operation = "" }, false, "no operation"},
		{"unknown operation", func(s *Step) { s.Operation = "delete" }, false, "supports"},
		{"export without document_id", func(s *Step) { s.DocumentID = "" }, false, "document_id"},
		{"unknown format", func(s *Step) { s.Format = "rtf" }, false, "offers"},
		{"broken document_id template", func(s *Step) { s.DocumentID = "{{ .Input " }, false, "template"},
		{"create without a title", func(s *Step) {
			s.Operation, s.DocumentID, s.Format = DocsOperationCreate, "", ""
		}, false, "title"},
		{"broken body template", func(s *Step) {
			s.Operation, s.DocumentID, s.Format = DocsOperationCreate, "", ""
			s.Title, s.Body = "t", "{{ .Previous "
		}, false, "template"},

		// Fields belonging to the other operation, or to another step type,
		// are refused rather than silently ignored — an author who set them
		// meant something by it.
		{"export with a title", func(s *Step) { s.Title = "nope" }, false, "another operation"},
		{"create with a format", func(s *Step) {
			s.Operation, s.DocumentID = DocsOperationCreate, ""
			s.Title, s.Format = "t", "pdf"
		}, false, "another operation"},
		{"carries a prompt", func(s *Step) { s.Prompt = "hello" }, false, "prompt"},
		{"carries code", func(s *Step) { s.Code = "function run(){}" }, false, "code"},
		{"attaches files", func(s *Step) { s.Attach = []string{"input.doc"} }, false, "attach"},
		{"declares an output_schema", func(s *Step) {
			s.OutputSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
		}, false, "fixed by its operation"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := ValidateSteps([]Step{docsStep(tc.mutate)})
			if ok != tc.ok {
				t.Fatalf("ok = %v (%s), want %v", ok, msg, tc.ok)
			}
			if !ok && !strings.Contains(strings.ToLower(msg), strings.ToLower(tc.says)) {
				t.Errorf("message %q does not mention %q", msg, tc.says)
			}
		})
	}
}

func TestProvidersAreDistinct(t *testing.T) {
	got := Providers([]Step{
		{Provider: "google"}, {Type: StepTypeCode}, {Provider: "google"}, {Provider: "jira"},
	})
	if len(got) != 2 || got[0] != "google" || got[1] != "jira" {
		t.Errorf("Providers = %v, want [google jira]", got)
	}
}

// An export's output must carry a file descriptor, because the whole point is
// that a later agent step can attach it.
func TestDocsOutputSchemaExportCarriesAFile(t *testing.T) {
	schema := DocsOutputSchema(DocsOperationExport)
	if len(schema) == 0 {
		t.Fatalf("export has no output schema")
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		t.Fatalf("export schema is not valid JSON: %v", err)
	}
	if _, has := doc.Properties["file"]; !has {
		t.Errorf("export schema declares no file")
	}
	if !strings.Contains(string(doc.Properties["file"]), "$file") {
		t.Errorf("the declared file is not a file descriptor: %s", doc.Properties["file"])
	}
	// It is a schema an editor will compile.
	if _, err := CompileSchema(schema); err != nil {
		t.Errorf("export schema does not compile: %v", err)
	}
	if _, err := CompileSchema(DocsOutputSchema(DocsOperationCreate)); err != nil {
		t.Errorf("create schema does not compile: %v", err)
	}
}

// The shape must be resolvable without anyone declaring it, and reachable by
// the next step — that is what lets an editor complete ctx.steps.brief.file.
func TestResolveShapesCoversGoogleDocs(t *testing.T) {
	got, err := ResolveShapes(Workflow{ID: "w", Steps: []Step{
		docsStep(nil),
		{Name: "read", Type: StepTypeAgent, AgentID: "a1", Prompt: "hi", Attach: []string{"steps.brief.file"}},
	}}, nil)
	if err != nil {
		t.Fatalf("ResolveShapes: %v", err)
	}
	if got.Steps[0].OutputSource != ShapeSourceOperation {
		t.Errorf("output_source = %q, want %q", got.Steps[0].OutputSource, ShapeSourceOperation)
	}
	// The agent step after it can see the exported file in its context.
	var ctxSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(got.Steps[1].ContextSchema, &ctxSchema); err != nil {
		t.Fatalf("decode context schema: %v", err)
	}
	if !strings.Contains(string(ctxSchema.Properties["steps"]), "$file") {
		t.Errorf("the agent step's context does not carry the exported file: %s", ctxSchema.Properties["steps"])
	}
}

func googleStepFixture(stepType, operation string, mutate func(*Step)) Step {
	step := Step{Name: "s", Type: stepType, Provider: "google", Operation: operation}
	switch operation {
	case GoogleOperationExport:
		step.DocumentID = "{{ .Input.id }}"
	case GoogleOperationCreate:
		step.Title = "T"
	case GoogleOperationAppend:
		step.DocumentID, step.Rows = "{{ .Input.id }}", `[["a",1]]`
	}
	if mutate != nil {
		mutate(&step)
	}
	return step
}

// Every type exports and creates; only a spreadsheet appends.
func TestGoogleStepOperationsByType(t *testing.T) {
	for _, tc := range []struct {
		stepType  string
		operation string
		ok        bool
	}{
		{StepTypeGoogleDocs, GoogleOperationExport, true},
		{StepTypeGoogleDocs, GoogleOperationCreate, true},
		{StepTypeGoogleDocs, GoogleOperationAppend, false},
		{StepTypeGoogleSheets, GoogleOperationExport, true},
		{StepTypeGoogleSheets, GoogleOperationCreate, true},
		{StepTypeGoogleSheets, GoogleOperationAppend, true},
		{StepTypeGoogleSlides, GoogleOperationExport, true},
		{StepTypeGoogleSlides, GoogleOperationCreate, true},
		{StepTypeGoogleSlides, GoogleOperationAppend, false},
	} {
		t.Run(tc.stepType+"/"+tc.operation, func(t *testing.T) {
			msg, ok := ValidateSteps([]Step{googleStepFixture(tc.stepType, tc.operation, nil)})
			if ok != tc.ok {
				t.Fatalf("ok = %v (%s), want %v", ok, msg, tc.ok)
			}
			if !ok && !strings.Contains(msg, "supports") {
				t.Errorf("message %q does not say what the type supports", msg)
			}
		})
	}
}

// Export formats differ per type, and a format from the wrong one is refused
// with a message naming what would have worked.
func TestGoogleExportFormatsAreTypeSpecific(t *testing.T) {
	for _, tc := range []struct {
		stepType string
		format   string
		ok       bool
	}{
		{StepTypeGoogleDocs, "docx", true},
		{StepTypeGoogleDocs, "xlsx", false},
		{StepTypeGoogleSheets, "csv", true},
		{StepTypeGoogleSheets, "xlsx", true},
		{StepTypeGoogleSheets, "docx", false},
		{StepTypeGoogleSlides, "pptx", true},
		{StepTypeGoogleSlides, "csv", false},
	} {
		t.Run(tc.stepType+"/"+tc.format, func(t *testing.T) {
			msg, ok := ValidateSteps([]Step{googleStepFixture(tc.stepType, GoogleOperationExport, func(s *Step) {
				s.Format = tc.format
			})})
			if ok != tc.ok {
				t.Fatalf("ok = %v (%s), want %v", ok, msg, tc.ok)
			}
			if !ok && !strings.Contains(msg, "offers") {
				t.Errorf("message %q does not list the formats that would work", msg)
			}
		})
	}
}

// A deck as pptx is downloadable but no model can read it, so the platform
// should know that rather than discovering it when an attach fails.
func TestSlidesPptxIsNotAttachable(t *testing.T) {
	pptx, ok := GoogleExportFormat(StepTypeGoogleSlides, "pptx")
	if !ok {
		t.Fatalf("slides does not offer pptx")
	}
	if pptx.Attachable {
		t.Errorf("pptx is marked attachable, but no model reads it")
	}
	pdf, _ := GoogleExportFormat(StepTypeGoogleSlides, "pdf")
	if !pdf.Attachable {
		t.Errorf("pdf should be attachable")
	}
}

// Fields belonging to another type or operation are refused rather than
// silently ignored: an author who set them meant something by it.
func TestGoogleStepRejectsMisplacedFields(t *testing.T) {
	cases := []struct {
		name string
		step Step
		says string
	}{
		{"rows on a document", googleStepFixture(StepTypeGoogleDocs, GoogleOperationCreate, func(s *Step) {
			s.Rows = `[["a"]]`
		}), "only a spreadsheet"},
		{"body on a spreadsheet", googleStepFixture(StepTypeGoogleSheets, GoogleOperationCreate, func(s *Step) {
			s.Body = "text"
		}), "rows"},
		{"body on a deck", googleStepFixture(StepTypeGoogleSlides, GoogleOperationCreate, func(s *Step) {
			s.Body = "text"
		}), "created empty"},
		{"range on an export", googleStepFixture(StepTypeGoogleSheets, GoogleOperationExport, func(s *Step) {
			s.Range = "A:C"
		}), "another operation"},
		{"append without rows", googleStepFixture(StepTypeGoogleSheets, GoogleOperationAppend, func(s *Step) {
			s.Rows = ""
		}), "rows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := ValidateSteps([]Step{tc.step})
			if ok {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(msg, tc.says) {
				t.Errorf("message %q does not mention %q", msg, tc.says)
			}
		})
	}
}

// Each type and operation advertises a shape an editor can compile.
func TestGoogleOutputSchemasCompile(t *testing.T) {
	for _, stepType := range []string{StepTypeGoogleDocs, StepTypeGoogleSheets, StepTypeGoogleSlides} {
		for _, operation := range []string{GoogleOperationExport, GoogleOperationCreate, GoogleOperationAppend} {
			schema := GoogleOutputSchema(stepType, operation)
			if len(schema) == 0 {
				continue
			}
			if _, err := CompileSchema(schema); err != nil {
				t.Errorf("%s/%s produced an uncompilable schema: %v", stepType, operation, err)
			}
		}
	}
	// An append reports where the rows landed, which is the only way to tell
	// a "log this" step worked.
	if !strings.Contains(string(GoogleOutputSchema(StepTypeGoogleSheets, GoogleOperationAppend)), "updated_range") {
		t.Errorf("append does not report where the rows went")
	}
}
