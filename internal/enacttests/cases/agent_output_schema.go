package cases

import (
	"encoding/json"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// agentOutputSchemaCase covers the lifecycle of an agent's structured-output
// schema: set at creation, replaced by an update, cleared with {}, and
// rejected when it is not a JSON Schema object.
//
// The schema is asserted semantically rather than byte-for-byte — it makes a
// round trip through OpenSearch, which is free to reorder object keys.
type agentOutputSchemaCase struct {
	agent utils.AgentDTO
}

func NewAgentOutputSchema() utils.TestCase { return &agentOutputSchemaCase{} }

func (c *agentOutputSchemaCase) Name() string { return "TestAgentManagement_OutputSchema" }

// Bedrock requires every object node to set additionalProperties:false, so a
// fixture without it is not a schema any agent could run with.
const createdSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`

func (c *agentOutputSchemaCase) Setup(t *utils.T) {
	c.agent = t.CreateAgent(`{"name":"integration test agent","model":"claude-sonnet-4-6",` +
		`"output_schema":` + createdSchema + `}`)
}

func (c *agentOutputSchemaCase) Run(t *utils.T) {
	if !sameJSON(c.agent.OutputSchema, createdSchema) {
		t.Fatalf("create: output_schema = %s, want %s", c.agent.OutputSchema, createdSchema)
	}

	// It must survive an update that does not mention it — the same
	// partial-update rule as every other field.
	kept := c.put(t, "update without output_schema", `{"system_prompt":"changed"}`, http.StatusOK)
	if !sameJSON(kept.OutputSchema, createdSchema) {
		t.Errorf("update without output_schema: schema = %s, want it unchanged", kept.OutputSchema)
	}

	// Replaced when provided.
	const replacement = `{"type":"object","properties":{"score":{"type":"number"}},"additionalProperties":false}`
	replaced := c.put(t, "replace output_schema", `{"output_schema":`+replacement+`}`, http.StatusOK)
	if !sameJSON(replaced.OutputSchema, replacement) {
		t.Errorf("replace output_schema: schema = %s, want %s", replaced.OutputSchema, replacement)
	}

	// A schema that is not an object is refused, and the message says so
	// rather than surfacing later as a Bedrock error.
	rejected := c.put(t, "non-object output_schema", `{"output_schema":["not","a","schema"]}`, http.StatusBadRequest)
	if !strings.Contains(rejected.Error, "output_schema") {
		t.Errorf("non-object output_schema: error %q does not name the field", rejected.Error)
	}

	// An object without additionalProperties:false is refused HERE. Bedrock
	// rejects it too, but only once a generation is under way — which reaches
	// whoever is talking to the agent, quoting a field name they have never
	// heard of.
	missing := c.put(t, "object without additionalProperties",
		`{"output_schema":{"type":"object","properties":{"a":{"type":"string"}}}}`, http.StatusBadRequest)
	if !strings.Contains(missing.Error, "additionalProperties") {
		t.Errorf("missing additionalProperties: error %q does not name the rule", missing.Error)
	}
	// Nested objects are covered too, and the message locates the one at fault.
	nested := c.put(t, "nested object without additionalProperties",
		`{"output_schema":{"type":"object","additionalProperties":false,"properties":{
			"outer":{"type":"object","properties":{"b":{"type":"string"}}}}}}`, http.StatusBadRequest)
	if !strings.Contains(nested.Error, "outer") {
		t.Errorf("nested violation: error %q does not locate the offending object", nested.Error)
	}

	// {} clears it. A null would be indistinguishable from an absent field,
	// which is why the empty object is the sentinel.
	cleared := c.put(t, "clear output_schema", `{"output_schema":{}}`, http.StatusOK)
	if len(cleared.OutputSchema) != 0 {
		t.Errorf("clear output_schema: schema = %s, want it absent", cleared.OutputSchema)
	}

	// And the clear is durable, not just an artefact of the update reply.
	var fetched utils.AgentDTO
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet, t.AgentURL("/v1/agents/"+c.agent.ID), nil, &fetched)
	if status != http.StatusOK {
		t.Fatalf("get after clear: got HTTP %d (%s), want 200", status, fetched.Error)
	}
	if len(fetched.OutputSchema) != 0 {
		t.Errorf("get after clear: schema = %s, want it absent", fetched.OutputSchema)
	}
}

// put sends one partial update and decodes the reply into a FRESH DTO.
//
// Reusing one struct across calls would quietly break the assertions here:
// encoding/json leaves a field absent from the payload untouched, so an
// omitted output_schema would read as the previous response's schema — which
// is exactly the value these assertions are trying to distinguish from.
func (c *agentOutputSchemaCase) put(t *utils.T, what, body string, want int) utils.AgentDTO {
	var out utils.AgentDTO
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPut,
		t.AgentURL("/v1/agents/"+c.agent.ID), strings.NewReader(body), &out)
	if status != want {
		t.Fatalf("%s: got HTTP %d (%s), want %d", what, status, out.Error, want)
	}
	return out
}

func (c *agentOutputSchemaCase) TearDown(t *utils.T) {
	t.DeleteAgent(c.agent.ID)
}

// sameJSON compares two JSON documents by value, so key order does not decide
// whether the test passes.
func sameJSON(got json.RawMessage, want string) bool {
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		return false
	}
	gotNorm, err := json.Marshal(a)
	if err != nil {
		return false
	}
	wantNorm, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(gotNorm) == string(wantNorm)
}
