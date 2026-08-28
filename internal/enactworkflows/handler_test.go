package enactworkflows

import (
	"encoding/json"
	"testing"
)

// An update merges: a field the caller did not mention keeps its stored value.
// That makes "absent" and "empty" different instructions, and a plain string
// cannot tell them apart — which is why Description is a pointer.
//
// Without this, a description could never be removed. Every empty one read as
// "leave it alone", the old text survived, and the caller was told the save
// succeeded.
func TestSaveRequestSeparatesAbsentFromEmptyDescription(t *testing.T) {
	for _, tc := range []struct {
		what      string
		body      string
		mentioned bool
		want      string
	}{
		{"absent", `{"name":"w"}`, false, ""},
		{"empty", `{"name":"w","description":""}`, true, ""},
		{"blank space", `{"name":"w","description":"   "}`, true, ""},
		{"set", `{"name":"w","description":"a workflow"}`, true, "a workflow"},
		{"null", `{"name":"w","description":null}`, false, ""},
	} {
		var body saveRequest
		if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
			t.Errorf("%s: decode: %v", tc.what, err)
			continue
		}
		if mentioned := body.Description != nil; mentioned != tc.mentioned {
			t.Errorf("%s: mentioned = %v, want %v", tc.what, mentioned, tc.mentioned)
		}
		if got := body.description(); got != tc.want {
			t.Errorf("%s: description() = %q, want %q", tc.what, got, tc.want)
		}
	}
}
