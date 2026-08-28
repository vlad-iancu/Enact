package workflows

import (
	"encoding/json"
	"strings"
	"testing"
)

const personSchema = `{
  "type": "object",
  "properties": {
    "customer": {"type": "string"},
    "n": {"type": "integer", "minimum": 1}
  },
  "required": ["customer"],
  "additionalProperties": false
}`

func TestCompileSchemaAcceptsAndRejects(t *testing.T) {
	if _, err := CompileSchema(json.RawMessage(personSchema)); err != nil {
		t.Errorf("a valid schema was rejected: %v", err)
	}
	// Absent means unconstrained, not forbidden.
	if s, err := CompileSchema(nil); err != nil || s != nil {
		t.Errorf("an absent schema should compile to nothing: %v %v", s, err)
	}
	for _, bad := range []struct {
		what   string
		schema string
		says   string
	}{
		{"array", `["not","a","schema"]`, "JSON object"},
		{"string", `"nope"`, "JSON object"},
		{"bad keyword type", `{"type": 5}`, "JSON Schema"},
		{"bad required", `{"required": "customer"}`, "JSON Schema"},
	} {
		if _, err := CompileSchema(json.RawMessage(bad.schema)); err == nil {
			t.Errorf("%s: compiled cleanly, want an error", bad.what)
		} else if !strings.Contains(err.Error(), bad.says) {
			t.Errorf("%s: error %q does not mention %q", bad.what, err, bad.says)
		}
	}
}

func TestValidateAgainstAcceptsAValidInstance(t *testing.T) {
	if err := ValidateAgainst(json.RawMessage(personSchema), json.RawMessage(`{"customer":"Ada","n":3}`)); err != nil {
		t.Errorf("a matching instance was rejected: %v", err)
	}
}

// The message has to be usable by whoever must fix the payload, which means
// naming the field rather than reproducing the schema.
func TestValidateAgainstNamesTheOffendingField(t *testing.T) {
	err := ValidateAgainst(json.RawMessage(personSchema), json.RawMessage(`{"customer": 5}`))
	if err == nil {
		t.Fatalf("a wrong type was accepted")
	}
	if !strings.Contains(err.Error(), "customer") {
		t.Errorf("error %q does not name the field", err)
	}
	if strings.Count(err.Error(), "\n") > 0 {
		t.Errorf("error spans multiple lines, which reads badly in an API response: %q", err)
	}
}

func TestValidateAgainstCatchesMissingAndExtraFields(t *testing.T) {
	if err := ValidateAgainst(json.RawMessage(personSchema), json.RawMessage(`{"n":1}`)); err == nil {
		t.Errorf("a missing required field was accepted")
	}
	if err := ValidateAgainst(json.RawMessage(personSchema), json.RawMessage(`{"customer":"Ada","extra":true}`)); err == nil {
		t.Errorf("an additional property was accepted despite additionalProperties:false")
	}
	if err := ValidateAgainst(json.RawMessage(personSchema), json.RawMessage(`{"customer":"Ada","n":0}`)); err == nil {
		t.Errorf("a value below the minimum was accepted")
	}
}

// Triggering with no payload at all must not slip past a schema that requires
// fields — otherwise the easiest way to bypass validation is to send nothing.
func TestValidateAgainstRejectsAnAbsentInstance(t *testing.T) {
	if err := ValidateAgainst(json.RawMessage(personSchema), nil); err == nil {
		t.Errorf("an absent payload satisfied a schema with required fields")
	}
}

func TestValidateAgainstIsANoOpWithoutASchema(t *testing.T) {
	if err := ValidateAgainst(nil, json.RawMessage(`{"anything":true}`)); err != nil {
		t.Errorf("no schema should accept anything: %v", err)
	}
}

// A code step declares its own output shape; an agent step must not, because
// its shape belongs to the agent.
func TestValidateStepsSchemaPlacement(t *testing.T) {
	msg, ok := ValidateSteps([]Step{{
		Name: "a", Type: StepTypeAgent, AgentID: "a1", Prompt: "hi",
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if ok {
		t.Errorf("an agent step was allowed to declare an output_schema")
	} else if !strings.Contains(msg, "agent") {
		t.Errorf("message %q does not explain where the schema belongs", msg)
	}

	if _, ok := ValidateSteps([]Step{{
		Name: "a", Type: StepTypeCode, Code: "function run(c){return c;}",
		OutputSchema: json.RawMessage(personSchema),
	}}); !ok {
		t.Errorf("a code step was not allowed to declare an output_schema")
	}

	msg, ok = ValidateSteps([]Step{{
		Name: "a", Type: StepTypeCode, Code: "function run(c){return c;}",
		OutputSchema: json.RawMessage(`{"type": 5}`),
	}})
	if ok {
		t.Errorf("an uncompilable output_schema was accepted at save time")
	} else if !strings.Contains(msg, "output_schema") {
		t.Errorf("message %q does not name the field", msg)
	}
}
