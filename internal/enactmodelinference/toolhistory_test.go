package enactmodelinference

import (
	"encoding/json"
	"testing"

	"enact/internal/logging"
	"enact/internal/tools"
)

func testBindings() map[string]toolBinding {
	return map[string]toolBinding{
		"github__list_issues": {Server: tools.Server{ID: "github"}, ToolName: "list_issues"},
	}
}

func TestExpandToolHistoryRebuildsThePair(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "what is open?"},
		{Role: "assistant", Content: "three issues are open", ToolCalls: []MessageToolCall{{
			ServerID: "github", Tool: "list_issues", ToolUseID: "use-1",
			Arguments: json.RawMessage(`{"repo":"enact"}`), Content: "issue A, issue B, issue C",
		}}},
		{Role: "user", Content: "and closed?"},
	}
	out := expandToolHistory(messages, testBindings(), logging.New())

	// One stored assistant message becomes three: the request, the results,
	// then the answer — in that order, because that is when they happened.
	if len(out) != 5 {
		t.Fatalf("got %d messages, want 5 (user, assistant+use, user+result, assistant text, user): %+v", len(out), out)
	}
	if out[1].Role != "assistant" || len(out[1].ToolUses) != 1 || out[1].Content != "" {
		t.Errorf("messages[1] = %+v, want an assistant message carrying only the tool use", out[1])
	}
	if use := out[1].ToolUses[0]; use.ID != "use-1" || use.Name != "github__list_issues" || string(use.Input) != `{"repo":"enact"}` {
		t.Errorf("tool use = %+v, want the bedrock tool name and the model's own arguments", use)
	}
	if out[2].Role != "user" || len(out[2].ToolResults) != 1 {
		t.Fatalf("messages[2] = %+v, want a user message carrying the tool result", out[2])
	}
	// Bedrock rejects a tool_use whose id is not answered in the very next
	// message, so this pairing is load-bearing.
	if got := out[2].ToolResults[0]; got.ToolUseID != "use-1" || got.Content != "issue A, issue B, issue C" {
		t.Errorf("tool result = %+v, want it keyed to the use it answers", got)
	}
	if out[3].Role != "assistant" || out[3].Content != "three issues are open" || len(out[3].ToolUses) != 0 {
		t.Errorf("messages[3] = %+v, want the answer as its own message", out[3])
	}
	// Roles still alternate, or the request is refused outright.
	for i := 1; i < len(out); i++ {
		if out[i].Role == out[i-1].Role {
			t.Errorf("messages %d and %d are both %q; roles must alternate", i-1, i, out[i].Role)
		}
	}
}

func TestExpandToolHistoryKeepsRoundsApart(t *testing.T) {
	bindings := testBindings()
	bindings["github__create_issue"] = toolBinding{Server: tools.Server{ID: "github"}, ToolName: "create_issue"}

	// Two rounds: the model looked, said something, then acted on what it
	// saw. Replaying both calls as one round would claim it decided to
	// create an issue before reading the list.
	out := expandToolHistory([]Message{{
		Role: "assistant", Content: "done",
		ToolCalls: []MessageToolCall{
			{ServerID: "github", Tool: "list_issues", ToolUseID: "u1", Turn: 1, Text: "let me look", Content: "none open"},
			{ServerID: "github", Tool: "create_issue", ToolUseID: "u2", Turn: 2, Text: "nothing there, filing one",
				StructuredContent: json.RawMessage(`{"number":7}`)},
		},
	}}, bindings, logging.New())

	if len(out) != 5 {
		t.Fatalf("got %d messages, want 5 (two rounds plus the answer): %+v", len(out), out)
	}
	if out[0].Content != "let me look" || len(out[0].ToolUses) != 1 || out[0].ToolUses[0].ID != "u1" {
		t.Errorf("round 1 = %+v, want its own words and only its own call", out[0])
	}
	if out[2].Content != "nothing there, filing one" || len(out[2].ToolUses) != 1 || out[2].ToolUses[0].ID != "u2" {
		t.Errorf("round 2 = %+v, want the second round alone", out[2])
	}
	// The structured result must survive as structure, not as a string.
	if got := out[3].ToolResults[0].Structured; string(got) != `{"number":7}` {
		t.Errorf("structured result = %q, want it carried through", got)
	}
	if out[4].Content != "done" {
		t.Errorf("final message = %q, want the answer", out[4].Content)
	}
	for i := 1; i < len(out); i++ {
		if out[i].Role == out[i-1].Role {
			t.Errorf("messages %d and %d are both %q; roles must alternate", i-1, i, out[i].Role)
		}
	}
}

func TestExpandToolHistoryEdgeCases(t *testing.T) {
	logger := logging.New()

	// A tool the agent no longer has: replaying its name would refer to a
	// tool this request never advertised, so the call goes and the answer
	// stays.
	out := expandToolHistory([]Message{
		{Role: "assistant", Content: "I looked it up", ToolCalls: []MessageToolCall{{
			ServerID: "removed-server", Tool: "gone", ToolUseID: "use-1",
		}}},
	}, testBindings(), logger)
	if len(out) != 1 || out[0].Content != "I looked it up" || len(out[0].ToolUses) != 0 {
		t.Errorf("unknown tool: got %+v, want the text alone with no tool use", out)
	}

	// No bindings at all (a raw model, or an agent whose tools were removed).
	out = expandToolHistory([]Message{
		{Role: "assistant", Content: "hi", ToolCalls: []MessageToolCall{{ServerID: "github", Tool: "list_issues"}}},
	}, nil, logger)
	if len(out) != 1 || len(out[0].ToolUses) != 0 {
		t.Errorf("no bindings: got %+v, want the text alone", out)
	}

	// A turn that ran a tool and produced no text: the pair is still
	// replayed, with no empty assistant message trailing it.
	out = expandToolHistory([]Message{
		{Role: "assistant", ToolCalls: []MessageToolCall{{
			ServerID: "github", Tool: "list_issues", ToolUseID: "use-2", Content: "none", IsError: true,
		}}},
	}, testBindings(), logger)
	if len(out) != 2 {
		t.Fatalf("empty answer: got %d messages, want 2: %+v", len(out), out)
	}
	if !out[1].ToolResults[0].IsError {
		t.Error("a failed tool result was replayed as a success")
	}
	// Absent arguments must still be valid JSON for the model to read.
	if string(out[0].ToolUses[0].Input) != "{}" {
		t.Errorf("missing arguments = %q, want {}", out[0].ToolUses[0].Input)
	}

	// Messages without tool calls are untouched.
	plain := []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
	if out := expandToolHistory(plain, testBindings(), logger); len(out) != 2 || out[1].Content != "hello" {
		t.Errorf("plain history was altered: %+v", out)
	}
}
