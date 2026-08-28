package bedrock

import (
	"encoding/json"
	"strings"
	"testing"
)

// The expectations here were established against the Bedrock API, not from
// documentation: every object node must set additionalProperties:false, and
// non-object roots are unconstrained.
func TestValidateStructuredOutputSchema(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		ok     bool
		says   string
	}{
		{"absent", ``, true, ""},
		{"root object with the flag", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`, true, ""},
		{"root object without it", `{"type":"object","properties":{"a":{"type":"string"}}}`, false, "additionalProperties"},
		{"root object with it true", `{"type":"object","properties":{},"additionalProperties":true}`, false, "requires false"},

		{"nested object without it", `{"type":"object","additionalProperties":false,
			"properties":{"outer":{"type":"object","properties":{"b":{"type":"string"}}}}}`, false, "properties.outer"},
		{"nested object with it", `{"type":"object","additionalProperties":false,
			"properties":{"outer":{"type":"object","properties":{"b":{"type":"string"}},"additionalProperties":false}}}`, true, ""},

		{"object in array items, without", `{"type":"object","additionalProperties":false,
			"properties":{"rows":{"type":"array","items":{"type":"object","properties":{"c":{"type":"string"}}}}}}`, false, "items"},
		{"object in array items, with", `{"type":"object","additionalProperties":false,
			"properties":{"rows":{"type":"array","items":{"type":"object","properties":{"c":{"type":"string"}},"additionalProperties":false}}}}`, true, ""},

		{"object inside anyOf", `{"type":"object","additionalProperties":false,"properties":{
			"v":{"anyOf":[{"type":"string"},{"type":"object","properties":{}}]}}}`, false, "anyOf[1]"},

		{"object in $defs", `{"type":"object","additionalProperties":false,"$defs":{
			"thing":{"type":"object","properties":{}}}}`, false, "$defs.thing"},

		// Bedrock accepts these, so this must too.
		{"array root", `{"type":"array","items":{"type":"string"}}`, true, ""},
		{"string root", `{"type":"string"}`, true, ""},
		{"required need not list everything", `{"type":"object","additionalProperties":false,
			"properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`, true, ""},

		// A property NAMED "type" or "items" is a field name, not a keyword.
		// Walking every map blindly would invent an error here.
		{"a property named type", `{"type":"object","additionalProperties":false,
			"properties":{"type":{"type":"string"},"items":{"type":"string"}}}`, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStructuredOutputSchema(json.RawMessage(tc.schema))
			if tc.ok && err != nil {
				t.Fatalf("rejected a schema Bedrock accepts: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("accepted a schema Bedrock rejects")
				}
				if !strings.Contains(err.Error(), tc.says) {
					t.Errorf("error %q does not point at %q", err, tc.says)
				}
			}
		})
	}
}

// The message must name the node, or someone with a large schema has no idea
// which object to fix.
func TestValidateStructuredOutputSchemaNamesTheNode(t *testing.T) {
	err := ValidateStructuredOutputSchema(json.RawMessage(`{"type":"object","additionalProperties":false,
		"properties":{"outer":{"type":"object","properties":{
			"inner":{"type":"object","properties":{},"additionalProperties":false}}}}}`))
	if err == nil {
		t.Fatalf("accepted a schema with an unflagged nested object")
	}
	if !strings.Contains(err.Error(), "properties.outer") {
		t.Errorf("error %q does not locate the offending object", err)
	}
}
