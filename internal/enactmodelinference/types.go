package enactmodelinference

import "encoding/json"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls are the MCP tools an assistant message used, replayed into
	// the model's context. See expandToolHistory for how one of these
	// becomes the assistant/user pair Bedrock requires.
	ToolCalls []MessageToolCall `json:"tool_calls,omitempty"`
}

// MessageToolCall is one recorded tool invocation being replayed.
//
// Turn groups calls into the rounds they were made in, and Text is the
// assistant's own words in that round — the model may say "let me check"
// before calling. Both are needed to replay the turn as it happened rather
// than as a summary of it.
type MessageToolCall struct {
	ServerID          string          `json:"server_id"`
	Tool              string          `json:"tool"`
	ToolUseID         string          `json:"tool_use_id"`
	Arguments         json.RawMessage `json:"arguments,omitempty"`
	Content           string          `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	IsError           bool            `json:"is_error,omitempty"`
	Turn              int             `json:"turn,omitempty"`
	Text              string          `json:"text,omitempty"`
}

type InferenceRequest struct {
	// AgentID, when set, runs the request through the named agent: the
	// agent's model and system prompt are used, and relevant context is
	// retrieved from its knowledge bases (RAG). When AgentID is set, Model
	// and SystemPrompt supplied on the request are ignored.
	AgentID      string    `json:"agent_id,omitempty"`
	Model        string    `json:"model,omitempty"`
	Messages     []Message `json:"messages"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	MaxTokens    int32     `json:"max_tokens,omitempty"`
	Temperature  *float32  `json:"temperature,omitempty"`
	TopP         *float32  `json:"top_p,omitempty"`
	Stream       bool      `json:"stream,omitempty"`
	// RetrievalTopK overrides the service-wide default for how many RAG
	// chunks are retrieved. Only meaningful when AgentID is set; distinct
	// from the model sampling parameters (top_p).
	RetrievalTopK *int `json:"retrieval_top_k,omitempty"`
	// ContextFiles are ad-hoc documents for THIS request only, passed to the
	// model natively as Bedrock DocumentBlocks (no server-side extraction).
	// Content carries the file's exact bytes base64-encoded; at most 5 files
	// of 4.5 MB each; supported formats: pdf, csv, doc, docx, xls, xlsx,
	// html, txt, md (derived from the filename extension).
	ContextFiles []ContextFile `json:"context_files,omitempty"`
}

// ContextFile is one ad-hoc document attached to an inference request.
type ContextFile struct {
	Filename string `json:"filename"`
	// Content is the file's raw bytes, base64-encoded (StdEncoding).
	Content string `json:"content"`
}

type InferenceResponse struct {
	Model   string `json:"model"`
	Content string `json:"content"`
	// ToolCalls records the MCP tool invocations executed during the
	// request's tool loop (non-streaming; streams announce them as
	// toolCall / toolCallResult SSE events instead).
	ToolCalls    []ToolCallRecord `json:"tool_calls,omitempty"`
	StopReason   string           `json:"stop_reason,omitempty"`
	InputTokens  int32            `json:"input_tokens"`
	OutputTokens int32            `json:"output_tokens"`
}

type errorResponse struct {
	Error string `json:"error"`
}
