package cases

import (
	"net/http"

	"enact/internal/enacttests/utils"
)

// mainAvatarValidationCase verifies the avatar upload endpoint's guard and
// validation. Deliberately no successful-upload path: that would couple the
// suite to AWS credentials and the S3 bucket; validation rejects before any
// storage call.
type mainAvatarValidationCase struct {
	utils.BaseCase
}

func NewMainAvatarValidation() utils.TestCase { return &mainAvatarValidationCase{} }

func (c *mainAvatarValidationCase) Name() string { return "TestMainAvatar_Validation" }

func (c *mainAvatarValidationCase) Run(t *utils.T) {
	// No session.
	if st := t.NewMainSession().DoMultipart(t, "/auth/me/avatar", "a.png", []byte("x"), nil); st != http.StatusUnauthorized {
		t.Errorf("no session: got HTTP %d, want 401", st)
	}

	s := t.NewMainSession()
	s.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	// Not an image (sniffed as text/plain regardless of the filename).
	var out struct {
		Error string `json:"error"`
	}
	if st := s.DoMultipart(t, "/auth/me/avatar", "avatar.png", []byte("just some text"), &out); st != http.StatusBadRequest {
		t.Errorf("non-image upload: got HTTP %d (%s), want 400", st, out.Error)
	}
}
