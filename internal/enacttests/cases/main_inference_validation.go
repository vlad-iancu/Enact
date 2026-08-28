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

	// The tuning parameters must survive enact-main's DTO. Both endpoints
	// decode with DisallowUnknownFields BEFORE any other validation, so a
	// body that names them and nothing to run against separates the two
	// outcomes without spending a single token: a field enact-main does not
	// know produces "unknown field", while one it forwards gets as far as the
	// target check.
	tuning := `"temperature":0.2,"top_p":0.9,"retrieval_top_k":3`
	assertForwarded(t, s, "/inference",
		`{"messages":[{"role":"user","content":"hi"}],`+tuning+`}`)
	// The conversation id is deliberately nonsense: decode happens first, so
	// this never reaches a lookup.
	assertForwarded(t, s, "/conversations/no-such-conversation/messages",
		`{"content":"hi",`+tuning+`}`)
}

// assertForwarded posts a body whose only defect is having no agent_id or
// model, and insists the refusal is about THAT rather than about a field
// enact-main failed to recognise.
func assertForwarded(t *utils.T, s *utils.MainSession, path, body string) {
	var out utils.InferenceErrDTO
	st := s.DoJSON(t, http.MethodPost, path, strings.NewReader(body), &out)
	if st != http.StatusBadRequest {
		t.Errorf("%s with tuning parameters: got HTTP %d, want 400", path, st)
		return
	}
	if strings.Contains(out.Error, "unknown field") {
		t.Errorf("%s does not accept a tuning parameter: %s", path, out.Error)
		return
	}
	if !strings.Contains(out.Error, "agent_id or model") {
		t.Errorf("%s: unexpected refusal %q; wanted the missing-target one", path, out.Error)
	}
}
