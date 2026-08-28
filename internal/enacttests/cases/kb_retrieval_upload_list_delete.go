package cases

import (
	"fmt"
	"net/http"
	"time"

	"enact/internal/enacttests/utils"
)

// kbRetrievalUploadListDeleteCase exercises the retrieval document pipeline
// end to end on a rag-kind knowledge base: upload (Tika + chunk + embed), the
// aggregation-backed listing (filename + chunk count), and queued deletion of
// one document's chunks.
//
// It also pins the decoupling itself: the knowledge base is attached to an
// agent, and the agent's own record is what names it. Documents never touch
// the agent API.
type kbRetrievalUploadListDeleteCase struct {
	kb    utils.KBDTO
	agent utils.AgentDTO
}

func NewKBRetrievalUploadListDelete() utils.TestCase { return &kbRetrievalUploadListDeleteCase{} }

func (c *kbRetrievalUploadListDeleteCase) Name() string {
	return "TestKBRetrieval_UploadListDelete"
}

func (c *kbRetrievalUploadListDeleteCase) Setup(t *utils.T) {
	c.kb = t.CreateKBOfKind("rag")
	c.agent = t.CreateAgent(fmt.Sprintf(
		`{"name":"rag test agent","model":"claude-sonnet-4-6","system_prompt":"test","rag_knowledge_base_id":%q}`,
		c.kb.ID))
}

func (c *kbRetrievalUploadListDeleteCase) Run(t *utils.T) {
	if c.agent.RAGKnowledgeBaseID != c.kb.ID {
		t.Fatalf("agent reports rag_knowledge_base_id %q, want %q", c.agent.RAGKnowledgeBaseID, c.kb.ID)
	}

	var upload struct {
		Documents []struct {
			DocumentID string `json:"document_id"`
		} `json:"documents"`
		Error string `json:"error"`
	}
	status := t.DoMultipart("enact-tests", utils.KBAudience, t.KBURL("/v1/knowledge-bases/"+c.kb.ID+"/documents"),
		"rag-lifecycle.txt", []byte("Sea otters hold hands while sleeping so they do not drift apart."), &upload)
	if status != http.StatusAccepted {
		t.Fatalf("upload: got HTTP %d (%s), want 202", status, upload.Error)
	}
	docID := upload.Documents[0].DocumentID
	t.Logf("uploaded retrieval document %s", docID)

	// Indexing embeds via Bedrock, so allow a generous window.
	t.Eventually(30*time.Second, "retrieval document appears on the kb detail", func() (bool, string) {
		var detail utils.KBDTO
		if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+c.kb.ID), nil, &detail); st != http.StatusOK {
			return false, fmt.Sprintf("detail returned HTTP %d", st)
		}
		for _, d := range detail.Documents {
			if d.DocumentID == docID {
				if d.Filename != "rag-lifecycle.txt" {
					return false, fmt.Sprintf("listed with filename %q", d.Filename)
				}
				if d.Chunks < 1 {
					return false, "listed with zero chunks"
				}
				return true, ""
			}
		}
		return false, "document not yet listed"
	})

	if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodDelete,
		t.KBURL("/v1/knowledge-bases/"+c.kb.ID+"/documents/"+docID), nil, nil); st != http.StatusAccepted {
		t.Fatalf("delete retrieval document: got HTTP %d, want 202", st)
	}

	t.Eventually(20*time.Second, "retrieval document disappears from the kb detail", func() (bool, string) {
		var detail utils.KBDTO
		if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+c.kb.ID), nil, &detail); st != http.StatusOK {
			return false, fmt.Sprintf("detail returned HTTP %d", st)
		}
		for _, d := range detail.Documents {
			if d.DocumentID == docID {
				return false, "document still listed"
			}
		}
		return true, ""
	})
}

func (c *kbRetrievalUploadListDeleteCase) TearDown(t *utils.T) {
	// The agent goes first, but only for tidiness: deleting it does NOT
	// cascade to the knowledge base, which is the point of the decoupling.
	// Deleting the knowledge base is what removes any remaining chunks.
	t.DeleteAgent(c.agent.ID)
	t.DeleteKB(c.kb.ID)
}
