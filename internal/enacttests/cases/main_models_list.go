package cases

import (
	"net/http"

	"enact/internal/enacttests/utils"
)

// mainModelsListCase verifies the model picker endpoint on enact-main:
// session-guarded and returning id + display name pairs.
type mainModelsListCase struct {
	utils.BaseCase
}

func NewMainModelsList() utils.TestCase { return &mainModelsListCase{} }

func (c *mainModelsListCase) Name() string { return "TestMainModels_List" }

func (c *mainModelsListCase) Run(t *utils.T) {
	if st := t.NewMainSession().DoJSON(t, http.MethodGet, "/models", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("no session -> /models: got HTTP %d, want 401", st)
	}

	s := t.NewMainSession()
	s.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	var out struct {
		Models []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/models", nil, &out); st != http.StatusOK {
		t.Fatalf("/models: got HTTP %d, want 200", st)
	}
	if len(out.Models) == 0 {
		t.Fatalf("/models: catalogue is empty")
	}
	for _, m := range out.Models {
		if m.ID == "" || m.DisplayName == "" {
			t.Errorf("model entry with empty id or display_name: %+v", m)
		}
	}
}
