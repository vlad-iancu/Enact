package enactmodelinference

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
}

type InferenceResponse struct {
	Model        string `json:"model"`
	Content      string `json:"content"`
	StopReason   string `json:"stop_reason,omitempty"`
	InputTokens  int32  `json:"input_tokens"`
	OutputTokens int32  `json:"output_tokens"`
}

type errorResponse struct {
	Error string `json:"error"`
}
