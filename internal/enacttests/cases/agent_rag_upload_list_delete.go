package cases

import (
	"fmt"
	"net/http"
	"time"

	"enact/internal/enacttests/utils"
)

// agentRAGUploadListDeleteCase exercises the RAG document pipeline: upload
// (Tika + chunk + embed), the aggregation-backed listing (filename + chunk
// count), and queued deletion of a single document's chunks.
type agentRAGUploadListDeleteCase struct {
	agent utils.AgentDTO
}

func NewAgentRAGUploadListDelete() utils.TestCase { return &agentRAGUploadListDeleteCase{} }

func (c *agentRAGUploadListDeleteCase) Name() string { return "TestAgentRAG_UploadListDelete" }

func (c *agentRAGUploadListDeleteCase) Setup(t *utils.T) {
	c.agent = t.CreateAgent(`{"name":"rag test agent","model":"claude-sonnet-4-6","system_prompt":"test"}`)
}

type ragDocsDTO struct {
	Documents []struct {
		DocumentID string `json:"document_id"`
		Filename   string `json:"filename"`
		Chunks     int    `json:"chunks"`
	} `json:"documents"`
	Error string `json:"error"`
}

func (c *agentRAGUploadListDeleteCase) Run(t *utils.T) {
	var upload struct {
		Documents []struct {
			DocumentID string `json:"document_id"`
		} `json:"documents"`
		Error string `json:"error"`
	}
	status := t.DoMultipart("enact-tests", utils.AgentAudience, t.AgentURL("/v1/agents/"+c.agent.ID+"/rag/documents"),
		"rag-lifecycle.txt", []byte("Sea otters hold hands while sleeping so they do not drift apart."), &upload)
	if status != http.StatusAccepted {
		t.Fatalf("upload: got HTTP %d (%s), want 202", status, upload.Error)
	}
	docID := upload.Documents[0].DocumentID
	t.Logf("uploaded rag document %s", docID)

	// Indexing embeds via Bedrock, so allow a generous window.
	t.Eventually(30*time.Second, "rag document appears in listing", func() (bool, string) {
		var list ragDocsDTO
		if st := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet, t.AgentURL("/v1/agents/"+c.agent.ID+"/rag/documents"), nil, &list); st != http.StatusOK {
			return false, fmt.Sprintf("listing returned HTTP %d", st)
		}
		for _, d := range list.Documents {
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

	if st := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodDelete,
		t.AgentURL("/v1/agents/"+c.agent.ID+"/rag/documents/"+docID), nil, nil); st != http.StatusAccepted {
		t.Fatalf("delete rag document: got HTTP %d, want 202", st)
	}

	t.Eventually(20*time.Second, "rag document disappears from listing", func() (bool, string) {
		var list ragDocsDTO
		if st := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet, t.AgentURL("/v1/agents/"+c.agent.ID+"/rag/documents"), nil, &list); st != http.StatusOK {
			return false, fmt.Sprintf("listing returned HTTP %d", st)
		}
		for _, d := range list.Documents {
			if d.DocumentID == docID {
				return false, "document still listed"
			}
		}
		return true, ""
	})
}

func (c *agentRAGUploadListDeleteCase) TearDown(t *utils.T) {
	// Agent deletion also cascades any remaining RAG chunks.
	t.DeleteAgent(c.agent.ID)
}
