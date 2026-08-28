package files

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRefReadsBothLayouts(t *testing.T) {
	for _, tc := range []struct {
		what string
		ref  string
		want Location
	}{
		{
			"execution-scoped",
			"fs:workflows/w1/executions/e9/files/2f1c",
			InExecution("w1", "e9"),
		},
		{
			"retained",
			"fs:workflows/w1/files/2f1c",
			InWorkflow("w1"),
		},
	} {
		scheme, loc, err := ParseRef(tc.ref)
		if err != nil {
			t.Errorf("%s: %v", tc.what, err)
			continue
		}
		if scheme != "fs" {
			t.Errorf("%s: scheme = %q, want fs", tc.what, scheme)
		}
		if loc != tc.want {
			t.Errorf("%s: location = %+v, want %+v", tc.what, loc, tc.want)
		}
	}
}

// A reference names bytes but carries no authority, so the parser is the last
// place a hostile one can be turned away cheaply. Everything here must be
// refused by shape rather than sanitised into something acceptable.
func TestParseRefRejectsAnythingItDidNotWrite(t *testing.T) {
	for _, tc := range []struct {
		what string
		ref  string
	}{
		{"no scheme", "workflows/w1/files/2f1c"},
		{"empty scheme", ":workflows/w1/files/2f1c"},
		{"traversal in the workflow id", "fs:workflows/../../etc/files/passwd"},
		{"traversal as a whole segment", "fs:workflows/w1/executions/../files/2f1c"},
		{"absolute path", "fs:/etc/passwd"},
		{"unknown layout", "fs:secrets/w1/files/2f1c"},
		{"execution layout missing a level", "fs:workflows/w1/executions/e9/2f1c"},
		{"trailing separator", "fs:workflows/w1/files/2f1c/"},
		{"double separator", "fs:workflows/w1//files/2f1c"},
		{"separator inside a segment", "fs:workflows/w1/files/2f1c%2Fx"},
		{"null byte", "fs:workflows/w1/files/2f1c\x00"},
		{"empty", ""},
	} {
		if _, _, err := ParseRef(tc.ref); err == nil {
			t.Errorf("%s: %q parsed cleanly, want a rejection", tc.what, tc.ref)
		}
	}
}

func TestLocationPrefixesDifferByScope(t *testing.T) {
	execution := InExecution("w1", "e9")
	if execution.IsRetained() {
		t.Error("an execution-scoped location reported itself as retained")
	}
	if got, want := execution.prefix(), "workflows/w1/executions/e9/files"; got != want {
		t.Errorf("prefix = %q, want %q", got, want)
	}

	retained := InWorkflow("w1")
	if !retained.IsRetained() {
		t.Error("a workflow-scoped location did not report itself as retained")
	}
	if got, want := retained.prefix(), "workflows/w1/files"; got != want {
		t.Errorf("prefix = %q, want %q", got, want)
	}
}

func TestLocationValidRejectsUnusableIDs(t *testing.T) {
	if InExecution("", "e9").Valid() {
		t.Error("an empty workflow id was accepted")
	}
	if InExecution("w1", "../e9").Valid() {
		t.Error("a traversal in the execution id was accepted")
	}
	if !InWorkflow("w1").Valid() {
		t.Error("a retained location with a good id was rejected")
	}
}

// The schema is what an editor completes against and what a step declares, so
// it has to describe the struct the runner actually writes.
func TestFileSchemaMatchesTheDescriptor(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(FileSchema(), &schema); err != nil {
		t.Fatalf("the file schema is not valid JSON: %v", err)
	}

	encoded, err := json.Marshal(File{Ref: "fs:workflows/w1/files/2f1c", MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("marshal a file: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode a file: %v", err)
	}
	for name := range fields {
		if _, described := schema.Properties[name]; !described {
			t.Errorf("the descriptor writes %q, which the schema does not describe", name)
		}
	}
	for _, name := range schema.Required {
		if _, present := fields[name]; !present {
			t.Errorf("the schema requires %q, which the descriptor omits", name)
		}
	}
	if !strings.Contains(string(FileSchema()), `"$file"`) {
		t.Error("the schema does not mention the $file marker")
	}
}
