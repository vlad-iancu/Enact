package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// mainInferenceValidationCase verifies the one-shot "test agent" inference
// wrapper on enact-main: session guard plus the request validations, all of
// which reject before any inference happens (zero token cost).
type mainInferenceValidationCase struct {
	utils.BaseCase
}

func NewMainInferenceValidation() utils.TestCase { return &mainInferenceValidationCase{} }

func (c *mainInferenceValidationCase) Name() string { return "TestMainInference_Validation" }

func (c *mainInferenceValidationCase) Run(t *utils.T) {
	if st := t.NewMainSession().DoJSON(t, http.MethodPost, "/inference",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`), nil); st != http.StatusUnauthorized {
		t.Errorf("no session: got HTTP %d, want 401", st)
	}

	s := t.NewMainSession()
	s.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	if st := s.DoJSON(t, http.MethodPost, "/inference",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`), nil); st != http.StatusBadRequest {
		t.Errorf("no target: got HTTP %d, want 400", st)
	}
	if st := s.DoJSON(t, http.MethodPost, "/inference",
		strings.NewReader(`{"model":"claude-sonnet-4-6","agent_id":"x","messages":[{"role":"user","content":"hi"}]}`), nil); st != http.StatusBadRequest {
		t.Errorf("both targets: got HTTP %d, want 400", st)
	}
	if st := s.DoJSON(t, http.MethodPost, "/inference",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`), nil); st != http.StatusBadRequest {
		t.Errorf("no messages: got HTTP %d, want 400", st)
	}
	entry := `{"filename":"f.txt","content":"aGk="}`
	six := strings.Repeat(entry+",", 5) + entry
	if st := s.DoJSON(t, http.MethodPost, "/inference",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"context_files":[`+six+`]}`), nil); st != http.StatusBadRequest {
		t.Errorf("six context files: got HTTP %d, want 400", st)
	}
}
