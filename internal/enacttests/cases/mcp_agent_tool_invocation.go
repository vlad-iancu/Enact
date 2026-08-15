package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

const agentToolServerID = "e2e-mcp-agent-tools"

// mcpAgentToolInvocationCase proves the full agent tool loop with a real
// model: an agent configured with the fixture MCP server is asked for the
// secret number, the model calls the tool (through the registry proxy), and
// the final answer contains the tool's result. Uses the cheapest model and
// a one-word answer to keep Bedrock cost minimal.
type mcpAgentToolInvocationCase struct {
	utils.BaseCase
	session *utils.MainSession
	agentID string
}

func NewMCPAgentToolInvocation() utils.TestCase { return &mcpAgentToolInvocationCase{} }

func (c *mcpAgentToolInvocationCase) Name() string { return "TestMCPAgent_ToolInvocation" }

func (c *mcpAgentToolInvocationCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E MCP Agent", mainTestEmail, mainTestPassword)
	c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+agentToolServerID, nil, nil)

	createBody := fmt.Sprintf(`{"id":%q,"url":%q,"transport_type":"streamable-http","description":"agent tools fixture"}`,
		agentToolServerID, t.Env.MCPFixtureURL)
	if st := c.session.DoJSON(t, http.MethodPost, "/mcp-servers", strings.NewReader(createBody), nil); st != http.StatusCreated {
		t.Fatalf("register mcp server: got HTTP %d, want 201", st)
	}

	// An agent referencing an unknown MCP server is rejected up front.
	badAgent := `{"name":"e2e bad tools","model":"claude-haiku-4-5","tools":["no-such-server"]}`
	if st := c.session.DoJSON(t, http.MethodPost, "/agents", strings.NewReader(badAgent), nil); st != http.StatusBadRequest {
		t.Errorf("create agent with unknown mcp server: got HTTP %d, want 400", st)
	}

	agentBody := fmt.Sprintf(`{"name":"e2e mcp agent","model":"claude-haiku-4-5","system_prompt":"You have tools. Use them when asked.","tools":[%q]}`, agentToolServerID)
	var agent struct {
		ID    string   `json:"id"`
		Tools []string `json:"tools"`
	}
	if st := c.session.DoJSON(t, http.MethodPost, "/agents", strings.NewReader(agentBody), &agent); st != http.StatusCreated {
		t.Fatalf("create agent with tools: got HTTP %d, want 201", st)
	}
	if len(agent.Tools) != 1 || agent.Tools[0] != agentToolServerID {
		t.Fatalf("created agent tools = %v, want [%s]", agent.Tools, agentToolServerID)
	}
	c.agentID = agent.ID
}

func (c *mcpAgentToolInvocationCase) Run(t *utils.T) {
	// Non-streaming inference straight at the inference service: the
	// response records the executed tool calls and the final answer must
	// carry the tool's result. (Streaming announces the same calls as
	// toolCall/toolCallResult SSE events; the loop under test is shared.)
	reqBody := fmt.Sprintf(`{"agent_id":%q,"messages":[{"role":"user","content":"Call the secret_number tool and reply with only the number it returns."}],"max_tokens":200}`, c.agentID)
	var out struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			ServerID string `json:"server_id"`
			Tool     string `json:"tool"`
			Content  string `json:"content"`
			IsError  bool   `json:"is_error"`
		} `json:"tool_calls"`
		StopReason string `json:"stop_reason"`
		Error      string `json:"error"`
	}
	status := t.DoJSON("enact-main", "enact-model-inference", http.MethodPost,
		t.Env.InferenceAPIURL+"/v1/inference", strings.NewReader(reqBody), &out)
	if status != http.StatusOK {
		t.Fatalf("inference with tools: got HTTP %d (%s), want 200", status, out.Error)
	}
	if len(out.ToolCalls) == 0 {
		t.Fatalf("inference executed no tool calls; content=%q", out.Content)
	}
	call := out.ToolCalls[0]
	if call.ServerID != agentToolServerID || call.Tool != "secret_number" {
		t.Errorf("tool call = %s/%s, want %s/secret_number", call.ServerID, call.Tool, agentToolServerID)
	}
	if call.IsError || call.Content != utils.SecretNumber {
		t.Errorf("tool call result = %q (is_error=%v), want %q", call.Content, call.IsError, utils.SecretNumber)
	}
	if !strings.Contains(out.Content, utils.SecretNumber) {
		t.Errorf("final answer %q does not contain the tool result %q", out.Content, utils.SecretNumber)
	}
	t.Logf("tool loop completed: %d call(s), stop_reason=%s", len(out.ToolCalls), out.StopReason)
}

func (c *mcpAgentToolInvocationCase) TearDown(t *utils.T) {
	if c.session == nil {
		return
	}
	if c.agentID != "" {
		c.session.DoJSON(t, http.MethodDelete, "/agents/"+c.agentID, nil, nil)
	}
	c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+agentToolServerID, nil, nil)
}
