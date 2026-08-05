package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
	"strings"
)

// inferenceRequiresMessagesCase verifies an inference request must carry at
// least one message.
type inferenceRequiresMessagesCase struct {
	utils.BaseCase
}

func NewInferenceRequiresMessages() utils.TestCase { return &inferenceRequiresMessagesCase{} }

func (c *inferenceRequiresMessagesCase) Name() string { return "TestInference_RequiresMessages" }

func (c *inferenceRequiresMessagesCase) Run(t *utils.T) {
	var out utils.InferenceErrDTO
	status := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost, t.Env.InferenceAPIURL+"/v1/inference",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`), &out)
	if status != http.StatusBadRequest {
		t.Fatalf("empty messages: got HTTP %d, want 400", status)
	}
}
