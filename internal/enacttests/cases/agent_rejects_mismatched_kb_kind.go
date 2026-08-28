package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// agentRejectsMismatchedKBKindCase pins that the two knowledge-base fields
// refuse each other's kind.
//
// Both directions fail silently at inference time if they are allowed, which
// is why they are refused at save time: a retrieval KB in knowledge_base_ids
// contributes a named but empty file to the prompt (its documents exist only
// as chunks, so the listing carries no text), and a context KB in
// rag_knowledge_base_id has no embeddings to search, so retrieval returns
// nothing — indistinguishable from "no relevant passage".
type agentRejectsMismatchedKBKindCase struct {
	contextKB utils.KBDTO
	ragKB     utils.KBDTO
}

func NewAgentRejectsMismatchedKBKind() utils.TestCase {
	return &agentRejectsMismatchedKBKindCase{}
}

func (c *agentRejectsMismatchedKBKindCase) Name() string {
	return "TestAgentManagement_RejectsMismatchedKBKind"
}

func (c *agentRejectsMismatchedKBKindCase) Setup(t *utils.T) {
	c.contextKB = t.CreateKBOfKind("context")
	c.ragKB = t.CreateKBOfKind("rag")
}

func (c *agentRejectsMismatchedKBKindCase) Run(t *utils.T) {
	// A retrieval KB offered as a context KB.
	c.expectRejected(t, "retrieval kb in knowledge_base_ids", fmt.Sprintf(
		`{"name":"mismatch a","model":"claude-sonnet-4-6","system_prompt":"p","knowledge_base_ids":[%q]}`,
		c.ragKB.ID))

	// A context KB offered as the retrieval one.
	c.expectRejected(t, "context kb in rag_knowledge_base_id", fmt.Sprintf(
		`{"name":"mismatch b","model":"claude-sonnet-4-6","system_prompt":"p","rag_knowledge_base_id":%q}`,
		c.contextKB.ID))

	// The right way round is still accepted — without this the case would
	// pass even if the agent API rejected every knowledge base outright.
	var created utils.AgentDTO
	body := fmt.Sprintf(
		`{"name":"correct","model":"claude-sonnet-4-6","system_prompt":"p","knowledge_base_ids":[%q],"rag_knowledge_base_id":%q}`,
		c.contextKB.ID, c.ragKB.ID)
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPost,
		t.AgentURL("/v1/agents"), strings.NewReader(body), &created)
	if status != http.StatusCreated {
		t.Fatalf("correctly-matched kinds: got HTTP %d (%s), want 201", status, created.Error)
	}
	if created.RAGKnowledgeBaseID != c.ragKB.ID {
		t.Errorf("created agent reports rag_knowledge_base_id %q, want %q", created.RAGKnowledgeBaseID, c.ragKB.ID)
	}
	t.DeleteAgent(created.ID)
}

// expectRejected asserts a create body is refused as a bad request, and that
// the refusal says something — a 400 with an empty message would leave the
// author guessing which of their knowledge bases was wrong.
func (c *agentRejectsMismatchedKBKindCase) expectRejected(t *utils.T, what, body string) {
	var out utils.AgentDTO
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPost,
		t.AgentURL("/v1/agents"), strings.NewReader(body), &out)
	if status != http.StatusBadRequest {
		t.Errorf("%s: got HTTP %d, want 400", what, status)
		// A 201 means an agent was created that nothing else will clean up.
		if out.ID != "" {
			t.DeleteAgent(out.ID)
		}
		return
	}
	if out.Error == "" {
		t.Errorf("%s: rejected with an empty error message", what)
	}
	t.Logf("%s rejected: %s", what, out.Error)
}

func (c *agentRejectsMismatchedKBKindCase) TearDown(t *utils.T) {
	t.DeleteKB(c.contextKB.ID)
	t.DeleteKB(c.ragKB.ID)
}
