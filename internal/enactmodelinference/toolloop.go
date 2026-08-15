package enactmodelinference

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"enact/internal/bedrock"
	"enact/internal/logging"
	"enact/internal/tools"
)

// toolBinding maps one Bedrock-visible tool name back to the MCP server and
// tool it stands for.
type toolBinding struct {
	Server   tools.Server
	ToolName string
}

// toolCallEvent is the SSE payload announcing one tool invocation the model
// requested; toolCallResultEvent reports its outcome. Both are named SSE
// events (toolCall / toolCallResult) so plain data-chunk consumers are
// unaffected.
type toolCallEvent struct {
	ServerID  string          `json:"server_id"`
	Tool      string          `json:"tool"`
	ToolUseID string          `json:"tool_use_id"`
	Arguments json.RawMessage `json:"arguments"`
	// Turn is the tool-loop round this call belongs to, counting from 1. A
	// consumer recording the exchange needs it: calls of one round were
	// requested together, and a later round's call was made knowing the
	// earlier round's results.
	Turn int `json:"turn"`
}

type toolCallResultEvent struct {
	ServerID   string          `json:"server_id"`
	Tool       string          `json:"tool"`
	ToolUseID  string          `json:"tool_use_id"`
	Content    string          `json:"content"`
	Structured json.RawMessage `json:"structured_content,omitempty"`
	IsError    bool            `json:"is_error"`
}

// ToolCallRecord is the non-streaming response's record of one executed
// tool call.
type ToolCallRecord struct {
	ServerID   string          `json:"server_id"`
	Tool       string          `json:"tool"`
	ToolUseID  string          `json:"tool_use_id"`
	Arguments  json.RawMessage `json:"arguments"`
	Content    string          `json:"content"`
	Structured json.RawMessage `json:"structured_content,omitempty"`
	IsError    bool            `json:"is_error"`
	Turn       int             `json:"turn"`
}

// bedrockToolName sanitizes to Bedrock's tool-name alphabet.
var bedrockToolNameInvalid = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// buildToolBindings resolves an agent's MCP server list into the Bedrock
// tool specs to advertise and the name→(server, tool) map needed to execute
// calls. Bedrock tool names are "<server>__<tool>", sanitized and
// de-duplicated, so distinct servers can expose same-named tools.
func (a *InferenceAPI) buildToolBindings(ctx context.Context, logger *logging.Logger, serverIDs []string) ([]bedrock.ToolSpec, map[string]toolBinding, error) {
	servers, err := a.tools.List(ctx, serverIDs, "")
	if err != nil {
		return nil, nil, fmt.Errorf("list MCP servers: %w", err)
	}
	byID := make(map[string]tools.Server, len(servers))
	for _, s := range servers {
		byID[s.ID] = s
	}
	for _, id := range serverIDs {
		if _, ok := byID[id]; !ok {
			// The server was deleted after the agent was configured; degrade
			// to the remaining toolset rather than failing the inference.
			logger.Warn("agent references missing mcp server", "server_id", id)
		}
	}
	defs, err := a.tools.ListTools(ctx, serverIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list MCP tools: %w", err)
	}

	specs := make([]bedrock.ToolSpec, 0, len(defs))
	bindings := make(map[string]toolBinding, len(defs))
	for _, def := range defs {
		server, ok := byID[def.ServerID]
		if !ok {
			continue
		}
		name := sanitizeBedrockToolName(def.ServerID + "__" + def.Name)
		for i := 2; ; i++ {
			if _, taken := bindings[name]; !taken {
				break
			}
			name = sanitizeBedrockToolName(fmt.Sprintf("%s_%d", name, i))
		}
		bindings[name] = toolBinding{Server: server, ToolName: def.Name}
		specs = append(specs, bedrock.ToolSpec{
			Name:        name,
			Description: def.Description,
			InputSchema: def.InputSchema,
		})
	}
	logger.Info("tool bindings built", "servers", serverIDs, "tools", len(specs))
	return specs, bindings, nil
}

// sanitizeBedrockToolName maps a name onto Bedrock's [a-zA-Z0-9_-] rules,
// truncated to 64 characters.
func sanitizeBedrockToolName(name string) string {
	s := bedrockToolNameInvalid.ReplaceAllString(name, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		s = "tool"
	}
	return s
}

// executeToolUses runs every tool call of one turn and returns the results
// message to feed back to the model. emit (optional) publishes the
// toolCall / toolCallResult events as execution progresses.
func (a *InferenceAPI) executeToolUses(ctx context.Context, logger *logging.Logger, turn int, bindings map[string]toolBinding, uses []bedrock.ToolUse, emit func(event string, payload any) error) (bedrock.Message, []ToolCallRecord, error) {
	results := bedrock.Message{Role: "user"}
	records := make([]ToolCallRecord, 0, len(uses))

	// Phase 1: resolve every credentialed tool up front, so a turn needing
	// two authorizations can ask for both at once instead of making the
	// user connect one account, wait, and then connect another.
	pending := a.resolveTurn(ctx, logger, bindings, uses, emit)
	// One deadline for the whole turn: N tools must not multiply how long
	// a single request can hang.
	turnDeadline := time.Now().Add(a.toolAuth.waitFor)

	for _, use := range uses {
		binding, known := bindings[use.Name]
		serverID, toolName := binding.Server.ID, binding.ToolName
		if !known {
			serverID, toolName = "", use.Name
		}
		if emit != nil {
			// The model's ORIGINAL arguments: injected credentials must
			// never reach the client or the conversation record.
			if err := emit("toolCall", toolCallEvent{
				ServerID:  serverID,
				Tool:      toolName,
				ToolUseID: use.ID,
				Arguments: use.Input,
				Turn:      turn,
			}); err != nil {
				return bedrock.Message{}, nil, err
			}
		}
		logger.Info("executing tool call", "server_id", serverID, "tool", toolName, "tool_use_id", use.ID, "arguments_bytes", len(use.Input))

		var content string
		var structured json.RawMessage
		isError := false
		switch {
		case !known:
			content = fmt.Sprintf("unknown tool %q", use.Name)
			isError = true
			logger.Warn("model requested unknown tool", "name", use.Name)
		default:
			arguments := use.Input
			var callOpts tools.CallOptions

			if state, credentialed := pending[use.ID]; credentialed {
				var err error
				state, err = a.settleAuthorization(ctx, logger, binding, use, state, turnDeadline, emit)
				if err != nil {
					return bedrock.Message{}, nil, err
				}
				switch {
				case state.Fatal != nil:
					content = fmt.Sprintf("tool authorization failed: %v", state.Fatal)
					isError = true
				case len(state.Missing) > 0:
					content = missingMessage(state.Missing)
					isError = true
					logger.Warn("tool call skipped: authorization not completed",
						"server_id", serverID, "tool", toolName, "missing", len(state.Missing))
				default:
					auth, _ := binding.Server.Authorization(binding.ToolName)
					headers, params, err := tools.Render(auth, state.Credentials)
					if err != nil {
						// The credential exists; the configuration is wrong.
						// Waiting cannot fix that.
						content = fmt.Sprintf("tool authorization could not be rendered: %v", err)
						isError = true
						logger.Error("failed to render tool authorization", "server_id", serverID, "tool", toolName, "err", err)
						break
					}
					merged, overwritten, err := tools.MergeParams(use.Input, params)
					if err != nil {
						content = fmt.Sprintf("tool arguments could not be prepared: %v", err)
						isError = true
						logger.Error("failed to merge credential params", "server_id", serverID, "tool", toolName, "err", err)
						break
					}
					arguments = merged
					callOpts.Headers = headers
					// Names only, never values (ADR-0008).
					logger.Info("tool credentials injected", "server_id", serverID, "tool", toolName,
						"headers", tools.HeaderNames(headers), "params", sortedKeys(params), "overwritten_params", overwritten)
				}
			}

			// The server's own gate, if it has one: the owner's credentials
			// for the handshake, underneath whatever the tool sends. The
			// tool's headers win a name clash — inside the door, the caller
			// is who they are.
			if !isError {
				probe, err := a.toolAuth.probeHeaders(ctx, logger, binding.Server)
				switch {
				case err != nil:
					content = fmt.Sprintf("tool call failed: %v", err)
					isError = true
					logger.Warn("session probe credentials unavailable", "server_id", serverID, "err", err)
				case len(probe) > 0:
					merged := make(map[string]string, len(probe)+len(callOpts.Headers))
					for name, value := range probe {
						merged[name] = value
					}
					for name, value := range callOpts.Headers {
						merged[name] = value
					}
					callOpts.Headers = merged
				}
			}

			if !isError {
				res, err := a.tools.CallTool(ctx, binding.Server, binding.ToolName, arguments, callOpts)
				if err != nil {
					content = fmt.Sprintf("tool call failed: %v", err)
					isError = true
					logger.Warn("tool call failed", "server_id", serverID, "tool", toolName, "err", err)
				} else {
					content = res.Content
					structured = res.StructuredContent
					isError = res.IsError
					logger.Info("tool call completed", "server_id", serverID, "tool", toolName,
						"content_chars", len(content), "structured_bytes", len(structured), "is_error", isError)
				}
			}
		}
		// Only truly empty results need a placeholder: a tool that returned
		// structured data and no prose has said something.
		if strings.TrimSpace(content) == "" && len(structured) == 0 {
			content = "(empty result)"
		}

		if emit != nil {
			if err := emit("toolCallResult", toolCallResultEvent{
				ServerID:   serverID,
				Tool:       toolName,
				ToolUseID:  use.ID,
				Content:    content,
				Structured: structured,
				IsError:    isError,
			}); err != nil {
				return bedrock.Message{}, nil, err
			}
		}
		results.ToolResults = append(results.ToolResults, bedrock.ToolResult{
			ToolUseID:  use.ID,
			Content:    content,
			Structured: structured,
			IsError:    isError,
		})
		records = append(records, ToolCallRecord{
			ServerID:  serverID,
			Tool:      toolName,
			ToolUseID: use.ID,
			// Pre-injection arguments: records are returned to clients and
			// recorded in conversations.
			Arguments:  use.Input,
			Content:    content,
			Structured: structured,
			IsError:    isError,
			Turn:       turn,
		})
	}
	return results, records, nil
}

// resolveTurn resolves credentials for every tool in the turn that declares
// requirements, and announces the ones that cannot proceed yet. Tools
// without requirements never reach the identity service at all.
func (a *InferenceAPI) resolveTurn(ctx context.Context, logger *logging.Logger, bindings map[string]toolBinding, uses []bedrock.ToolUse, emit func(event string, payload any) error) map[string]resolved {
	pending := map[string]resolved{}
	for _, use := range uses {
		binding, known := bindings[use.Name]
		if !known || len(binding.Server.Requirements(binding.ToolName)) == 0 {
			continue
		}
		state := a.toolAuth.resolve(ctx, logger, binding.Server, binding.ToolName)
		pending[use.ID] = state
		// Announce every unmet call before executing any of them, so the UI
		// can render all the "connect" prompts at once.
		if emit != nil && state.Fatal == nil && len(state.Missing) > 0 {
			_ = emit("toolCallWaitingAuthorization", toolCallWaitingAuthorizationEvent{
				ServerID:    binding.Server.ID,
				Tool:        binding.ToolName,
				ToolUseID:   use.ID,
				Status:      waitStatusWaiting,
				Missing:     state.Missing,
				WaitSeconds: int(a.toolAuth.waitFor.Seconds()),
			})
		}
	}
	return pending
}

// settleAuthorization waits for a call's missing credentials when there is
// a stream to tell the user through. Without one (the non-streaming path)
// it returns immediately: a silent multi-minute stall with no way to say
// what is missing is worse than an honest error.
func (a *InferenceAPI) settleAuthorization(ctx context.Context, logger *logging.Logger, binding toolBinding, use bedrock.ToolUse, state resolved, deadline time.Time, emit func(event string, payload any) error) (resolved, error) {
	if state.Fatal != nil || len(state.Missing) == 0 {
		return state, nil
	}
	if emit == nil {
		logger.Warn("authorization required but the response is not streaming; not waiting",
			"server_id", binding.Server.ID, "tool", binding.ToolName, "missing", len(state.Missing))
		return state, nil
	}
	settled, err := a.toolAuth.waitForAuthorization(ctx, logger, binding.Server, binding.ToolName, use.ID, state, deadline, emit)
	if err != nil {
		// A write failure or a cancelled request ends the whole stream.
		return settled, err
	}
	return settled, nil
}

// expandToolHistory rewrites the conversation the model is given so that a
// past assistant message which used tools is replayed the way it actually
// happened, in the three messages Bedrock's protocol requires:
//
//	assistant { tool_use }      what the model asked for
//	user      { tool_result }   what came back
//	assistant { text }          what it concluded
//
// Without this the model sees only the conclusion, so it cannot say what it
// searched for, and may re-run a tool it already ran. Bedrock validates the
// pairing strictly — a tool_use with no matching tool_result in the very
// next message is rejected — which is why both halves are built from one
// record and dropped together.
//
// A call whose tool is no longer in the agent's bindings is skipped: its
// name would refer to a tool this request never advertised. The final text
// still survives, so the turn is not lost, only its tool detail.
func expandToolHistory(messages []Message, bindings map[string]toolBinding, logger *logging.Logger) []bedrock.Message {
	byTool := make(map[string]string, len(bindings))
	for name, binding := range bindings {
		byTool[binding.Server.ID+"\x00"+binding.ToolName] = name
	}

	out := make([]bedrock.Message, 0, len(messages))
	replayed, skipped := 0, 0
	for _, m := range messages {
		if len(m.ToolCalls) == 0 {
			out = append(out, bedrock.Message{Role: m.Role, Content: m.Content})
			continue
		}
		// One round at a time, in the order they ran: a second round's call
		// was made KNOWING the first round's results, and collapsing them
		// into one request would tell the model something untrue about its
		// own reasoning.
		emitted := false
		for _, round := range groupByTurn(m.ToolCalls) {
			var uses []bedrock.ToolUse
			var results []bedrock.ToolResult
			text := ""
			for _, call := range round {
				if call.Text != "" {
					text = call.Text
				}
				name, known := byTool[call.ServerID+"\x00"+call.Tool]
				if !known {
					skipped++
					continue
				}
				arguments := call.Arguments
				if len(arguments) == 0 {
					arguments = json.RawMessage("{}")
				}
				uses = append(uses, bedrock.ToolUse{ID: call.ToolUseID, Name: name, Input: arguments})
				results = append(results, bedrock.ToolResult{
					ToolUseID:  call.ToolUseID,
					Content:    call.Content,
					Structured: call.StructuredContent,
					IsError:    call.IsError,
				})
			}
			if len(uses) == 0 {
				continue
			}
			out = append(out,
				bedrock.Message{Role: m.Role, Content: text, ToolUses: uses},
				bedrock.Message{Role: "user", ToolResults: results},
			)
			replayed += len(uses)
			emitted = true
		}
		// The answer is its own message after the last round — or the whole
		// message, when nothing could be replayed.
		if m.Content != "" || !emitted {
			out = append(out, bedrock.Message{Role: m.Role, Content: m.Content})
		}
	}
	if replayed > 0 || skipped > 0 {
		logger.Info("tool history replayed", "calls", replayed, "skipped_unknown_tools", skipped,
			"messages_in", len(messages), "messages_out", len(out))
	}
	return out
}

// groupByTurn splits a message's tool calls into the rounds they were made
// in, preserving order. Records written before rounds were tracked have no
// turn at all; they become a single round, which is what they were assumed
// to be anyway.
func groupByTurn(calls []MessageToolCall) [][]MessageToolCall {
	var rounds [][]MessageToolCall
	current := -1
	for _, call := range calls {
		if len(rounds) == 0 || call.Turn != current {
			rounds = append(rounds, nil)
			current = call.Turn
		}
		rounds[len(rounds)-1] = append(rounds[len(rounds)-1], call)
	}
	return rounds
}

// sortedKeys lists a map's keys for logging.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
