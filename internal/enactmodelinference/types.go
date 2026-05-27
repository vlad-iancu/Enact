package enactmodelinference

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type InferenceRequest struct {
	Model        string    `json:"model"`
	Messages     []Message `json:"messages"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	MaxTokens    int32     `json:"max_tokens,omitempty"`
	Temperature  *float32  `json:"temperature,omitempty"`
	TopP         *float32  `json:"top_p,omitempty"`
	Stream       bool      `json:"stream,omitempty"`
}

type InferenceResponse struct {
	Model        string `json:"model"`
	Content      string `json:"content"`
	StopReason   string `json:"stop_reason,omitempty"`
	InputTokens  int32  `json:"input_tokens"`
	OutputTokens int32  `json:"output_tokens"`
}

type StreamChunk struct {
	Delta        string `json:"delta,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	InputTokens  int32  `json:"input_tokens,omitempty"`
	OutputTokens int32  `json:"output_tokens,omitempty"`
	Done         bool   `json:"done,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}
