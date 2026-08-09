package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// mainConversationsLifecycleCase verifies the conversation surface through
// enact-main: create, rename via PUT, listing, get, message-request
// validation (the 400 paths, which never reach Bedrock so runs cost no
// tokens), and cross-user isolation.
//
// Conversations have no delete endpoint, so the fixtures this case creates
// remain — acceptable for a dev tool; they are titled distinctively.
type mainConversationsLifecycleCase struct {
	utils.BaseCase
}

func NewMainConversationsLifecycle() utils.TestCase { return &mainConversationsLifecycleCase{} }

func (c *mainConversationsLifecycleCase) Name() string { return "TestMainConversations_Lifecycle" }

func (c *mainConversationsLifecycleCase) Run(t *utils.T) {
	s := t.NewMainSession()
	s.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	var created struct {
		ID string `json:"id"`
	}
	if st := s.DoJSON(t, http.MethodPost, "/conversations", nil, &created); st != http.StatusCreated || created.ID == "" {
		t.Fatalf("create conversation: got HTTP %d id %q, want 201", st, created.ID)
	}
	convID := created.ID

	var renamed struct {
		Title string `json:"title"`
	}
	if st := s.DoJSON(t, http.MethodPut, "/conversations/"+convID, strings.NewReader(`{"title":"e2e conversation case"}`), &renamed); st != http.StatusOK || renamed.Title != "e2e conversation case" {
		t.Errorf("rename: HTTP %d title %q, want 200 %q", st, renamed.Title, "e2e conversation case")
	}
	if st := s.DoJSON(t, http.MethodPut, "/conversations/"+convID, strings.NewReader(`{"title":"  "}`), nil); st != http.StatusBadRequest {
		t.Errorf("blank title: got HTTP %d, want 400", st)
	}

	var list struct {
		Conversations []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"conversations"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/conversations", nil, &list); st != http.StatusOK {
		t.Fatalf("list: got HTTP %d, want 200", st)
	}
	found := false
	for _, cv := range list.Conversations {
		if cv.ID == convID && cv.Title == "e2e conversation case" {
			found = true
		}
	}
	if !found {
		t.Errorf("renamed conversation missing from listing (%d conversations)", len(list.Conversations))
	}

	// Message validation errors reject before any inference happens.
	if st := s.DoJSON(t, http.MethodPost, "/conversations/"+convID+"/messages", strings.NewReader(`{"content":"hi"}`), nil); st != http.StatusBadRequest {
		t.Errorf("message without target: got HTTP %d, want 400", st)
	}
	// Six context files exceed the pass-through cap — rejected as a clean
	// 400 before the SSE stream (and before any inference) starts.
	entry := `{"filename":"f.txt","content":"aGk="}`
	six := strings.Repeat(entry+",", 5) + entry
	if st := s.DoJSON(t, http.MethodPost, "/conversations/"+convID+"/messages",
		strings.NewReader(`{"content":"hi","model":"claude-sonnet-4-6","context_files":[`+six+`]}`), nil); st != http.StatusBadRequest {
		t.Errorf("six context files: got HTTP %d, want 400", st)
	}
	if st := s.DoJSON(t, http.MethodPost, "/conversations/"+convID+"/messages", strings.NewReader(`{"content":" ","model":"claude-sonnet-4-6"}`), nil); st != http.StatusBadRequest {
		t.Errorf("empty content: got HTTP %d, want 400", st)
	}

	// Another user cannot see or rename this conversation.
	other := t.NewMainSession()
	other.RegisterOrLogin(t, "E2E Other", mainOtherEmail, mainTestPassword)
	if st := other.DoJSON(t, http.MethodGet, "/conversations/"+convID, nil, nil); st != http.StatusNotFound {
		t.Errorf("other user get: got HTTP %d, want 404", st)
	}
	if st := other.DoJSON(t, http.MethodPut, "/conversations/"+convID, strings.NewReader(`{"title":"hijack"}`), nil); st != http.StatusNotFound {
		t.Errorf("other user rename: got HTTP %d, want 404", st)
	}
}
