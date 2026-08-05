package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
	"strings"
)

// inferenceRejectsUnknownModelCase verifies model validation on inference.
type inferenceRejectsUnknownModelCase struct {
	utils.BaseCase
}

func NewInferenceRejectsUnknownModel() utils.TestCase { return &inferenceRejectsUnknownModelCase{} }

func (c *inferenceRejectsUnknownModelCase) Name() string {
	return "TestInference_RejectsUnknownModel"
}

func (c *inferenceRejectsUnknownModelCase) Run(t *utils.T) {
	var out utils.InferenceErrDTO
	status := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost, t.Env.InferenceAPIURL+"/v1/inference",
		strings.NewReader(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`), &out)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown model: got HTTP %d, want 400", status)
	}
	if !strings.Contains(out.Error, "no-such-model") {
		t.Errorf("error %q does not name the rejected model", out.Error)
	}
}
