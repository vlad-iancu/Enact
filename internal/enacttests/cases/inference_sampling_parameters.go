package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// inferenceSamplingParametersCase covers temperature and top_p range
// validation at the inference service, which is the authority on what the
// models accept.
//
// Only the bounds are asserted, never an effect on the reply: sampling
// settings change the wording, which is precisely what a test cannot pin
// down. That enact-main forwards the fields at all is covered separately, in
// TestMainInference_Validation.
type inferenceSamplingParametersCase struct {
	utils.BaseCase
}

func NewInferenceSamplingParameters() utils.TestCase { return &inferenceSamplingParametersCase{} }

func (c *inferenceSamplingParametersCase) Name() string {
	return "TestInference_SamplingParameters"
}

func (c *inferenceSamplingParametersCase) Run(t *utils.T) {
	// The inference service bounds both to 0–1, and refuses both at once —
	// the models reject that combination, and a 400 naming it beats the
	// Bedrock ValidationException that would otherwise surface as a 502.
	for _, bad := range []struct{ field, body string }{
		{"temperature above 1", `"temperature":1.5`},
		{"temperature below 0", `"temperature":-0.1`},
		{"top_p above 1", `"top_p":1.5`},
		{"top_p below 0", `"top_p":-0.1`},
		{"both at once", `"temperature":0.2,"top_p":0.9`},
	} {
		var out utils.InferenceErrDTO
		body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],%s}`, bad.body)
		status := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost,
			t.Env.InferenceAPIURL+"/v1/inference", strings.NewReader(body), &out)
		if status != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", bad.field, status)
			continue
		}
		if out.Error == "" {
			t.Errorf("%s: rejected with an empty error message", bad.field)
		}
	}

	// One in-range parameter, on its own, reaches the model and comes back.
	// Without this the case would pass if the service started refusing every
	// request that named either field.
	for _, good := range []struct{ field, body string }{
		{"temperature alone", `"temperature":0`},
		{"top_p alone", `"top_p":0.9`},
	} {
		var out utils.InferenceErrDTO
		body := fmt.Sprintf(
			`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Reply with the single word: ok"}],"max_tokens":16,%s}`,
			good.body)
		status := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost,
			t.Env.InferenceAPIURL+"/v1/inference", strings.NewReader(body), &out)
		if status != http.StatusOK {
			t.Errorf("%s: got HTTP %d (%s), want 200", good.field, status, out.Error)
		}
	}
}
