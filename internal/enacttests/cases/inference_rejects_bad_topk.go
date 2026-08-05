package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
	"strings"
)

// inferenceRejectsBadTopKCase verifies retrieval_top_k range validation.
type inferenceRejectsBadTopKCase struct {
	utils.BaseCase
}

func NewInferenceRejectsBadTopK() utils.TestCase { return &inferenceRejectsBadTopKCase{} }

func (c *inferenceRejectsBadTopKCase) Name() string { return "TestInference_RejectsBadTopK" }

func (c *inferenceRejectsBadTopKCase) Run(t *utils.T) {
	var out utils.InferenceErrDTO
	status := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost, t.Env.InferenceAPIURL+"/v1/inference",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"retrieval_top_k":0}`), &out)
	if status != http.StatusBadRequest {
		t.Fatalf("retrieval_top_k=0: got HTTP %d, want 400", status)
	}
}
