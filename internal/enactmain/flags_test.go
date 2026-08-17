package enactmain

import (
	"encoding/json"
	"testing"

	"enact/internal/agents"
	"enact/internal/extidentities"
	"enact/internal/kb"
	"enact/internal/tools"
)

// TestFlagShapes locks the JSON contract the frontend reads: the action
// hints must appear as top-level keys beside the resource's own fields, not
// nested under an embedded object. Anonymous embedding is what flattens
// them, and it is silent if it ever stops being anonymous.
func TestFlagShapes(t *testing.T) {
	flags := resourceFlags{Editable: true, Deletable: false, Usable: true}
	// key is the resource's own identifying field: it must survive the
	// embedding alongside the flags. A provider is keyed by name, the rest
	// by id.
	for _, tc := range []struct {
		name string
		key  string
		v    any
	}{
		{"agent", "id", agentListItem{Agent: agents.Agent{ID: "a1", Name: "MyAssistant"}, resourceFlags: flags}},
		{"kb", "id", kbListItem{KnowledgeBase: kb.KnowledgeBase{ID: "k1", Name: "Docs"}, resourceFlags: flags}},
		{"mcp", "id", mcpServerItem{Server: tools.Server{ID: "s1", URL: "https://example"}, resourceFlags: flags}},
		{"provider", "name", identityProviderItem{ProviderSummary: extidentities.ProviderSummary{Name: "gmail"}, resourceFlags: flags}},
	} {
		raw, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var object map[string]any
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, key := range []string{"editable", "deletable", "usable", tc.key} {
			if _, ok := object[key]; !ok {
				t.Errorf("%s: missing %q in %s", tc.name, key, raw)
			}
		}
		if object["editable"] != true || object["deletable"] != false || object["usable"] != true {
			t.Errorf("%s: wrong flag values: %s", tc.name, raw)
		}
		t.Logf("%-9s %s", tc.name, raw)
	}
}

func TestWithFlagsPassthrough(t *testing.T) {
	body := []byte(`{"id":"k1","name":"Docs","documents":[{"filename":"a.pdf"}]}`)
	out := withFlags(body, resourceFlags{Editable: true, Usable: true})
	var object map[string]any
	if err := json.Unmarshal(out, &object); err != nil {
		t.Fatal(err)
	}
	if object["usable"] != true || object["editable"] != true || object["deletable"] != false {
		t.Errorf("flags not merged: %s", out)
	}
	if object["documents"] == nil || object["name"] != "Docs" {
		t.Errorf("passthrough fields lost: %s", out)
	}
	if got := string(withFlags([]byte("not json"), resourceFlags{})); got != "not json" {
		t.Errorf("non-JSON body should pass through unchanged, got %q", got)
	}
	t.Logf("merged %s", out)
}
