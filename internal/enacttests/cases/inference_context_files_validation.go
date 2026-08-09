package cases

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// inferenceContextFilesValidationCase verifies the context_files request
// validation: unsupported types, bad base64, and the file-count cap. All
// paths reject before any Bedrock call, so runs cost no tokens.
type inferenceContextFilesValidationCase struct {
	utils.BaseCase
}

func NewInferenceContextFilesValidation() utils.TestCase {
	return &inferenceContextFilesValidationCase{}
}

func (c *inferenceContextFilesValidationCase) Name() string {
	return "TestInference_ContextFilesValidation"
}

func (c *inferenceContextFilesValidationCase) request(files string) string {
	return fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"context_files":[%s]}`, files)
}

func (c *inferenceContextFilesValidationCase) Run(t *utils.T) {
	url := t.Env.InferenceAPIURL + "/v1/inference"
	valid := base64.StdEncoding.EncodeToString([]byte("hello"))

	var out utils.InferenceErrDTO
	if st := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost, url,
		strings.NewReader(c.request(`{"filename":"malware.exe","content":"`+valid+`"}`)), &out); st != http.StatusBadRequest {
		t.Errorf("unsupported extension: got HTTP %d, want 400", st)
	} else if !strings.Contains(out.Error, "unsupported") {
		t.Errorf("unsupported extension error %q lacks explanation", out.Error)
	}

	if st := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost, url,
		strings.NewReader(c.request(`{"filename":"notes.txt","content":"@@not-base64@@"}`)), nil); st != http.StatusBadRequest {
		t.Errorf("invalid base64: got HTTP %d, want 400", st)
	}

	// Six files exceed the five-file cap.
	entry := `{"filename":"f.txt","content":"` + valid + `"}`
	six := strings.Repeat(entry+",", 5) + entry
	if st := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost, url,
		strings.NewReader(c.request(six)), nil); st != http.StatusBadRequest {
		t.Errorf("six context files: got HTTP %d, want 400", st)
	}

	// Files without any user message to attach to are rejected.
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":"hi"}],"context_files":[` +
		`{"filename":"notes.txt","content":"` + valid + `"}]}`
	if st := t.DoJSON("enact-tests", utils.InferenceAudience, http.MethodPost, url,
		strings.NewReader(body), nil); st != http.StatusBadRequest {
		t.Errorf("no user message: got HTTP %d, want 400", st)
	}
}