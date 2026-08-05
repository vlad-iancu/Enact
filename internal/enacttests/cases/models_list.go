package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
)

// modelsListCase verifies the model catalogue is non-empty and well-formed.
type modelsListCase struct {
	utils.BaseCase
}

func NewModelsList() utils.TestCase { return &modelsListCase{} }

func (c *modelsListCase) Name() string { return "TestModels_ListNonEmpty" }

func (c *modelsListCase) Run(t *utils.T) {
	var out struct {
		Models []struct {
			Name           string `json:"name"`
			BedrockModelID string `json:"bedrock_model_id"`
		} `json:"models"`
		Error string `json:"error"`
	}
	status := t.DoJSON("enact-tests", "enact-model-management", http.MethodGet, t.Env.ModelsAPIURL+"/v1/models", nil, &out)
	if status != http.StatusOK {
		t.Fatalf("list models: got HTTP %d (%s), want 200", status, out.Error)
	}
	if len(out.Models) == 0 {
		t.Fatalf("list models: catalogue is empty")
	}
	for _, m := range out.Models {
		if m.Name == "" || m.BedrockModelID == "" {
			t.Errorf("model entry with empty name or bedrock_model_id: %+v", m)
		}
	}
}
