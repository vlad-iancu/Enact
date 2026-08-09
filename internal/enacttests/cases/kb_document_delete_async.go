package cases

import (
	"fmt"
	"net/http"
	"time"

	"enact/internal/enacttests/utils"
)

// kbDocumentDeleteAsyncCase exercises the asynchronous document pipeline end
// to end: upload (indexer stores the document), then queued deletion (the
// indexer removes it), asserting through the KB detail listing.
type kbDocumentDeleteAsyncCase struct {
	kb utils.KBDTO
}

func NewKBDocumentDeleteAsync() utils.TestCase { return &kbDocumentDeleteAsyncCase{} }

func (c *kbDocumentDeleteAsyncCase) Name() string { return "TestKB_DocumentDeleteAsync" }

func (c *kbDocumentDeleteAsyncCase) Setup(t *utils.T) {
	c.kb = t.CreateKB()
}

func (c *kbDocumentDeleteAsyncCase) Run(t *utils.T) {
	var upload struct {
		Documents []struct {
			DocumentID string `json:"document_id"`
		} `json:"documents"`
		Error string `json:"error"`
	}
	status := t.DoMultipart("enact-tests", utils.KBAudience, t.KBURL("/v1/knowledge-bases/"+c.kb.ID+"/documents"),
		"delete-test.txt", []byte("A short document destined for deletion."), &upload)
	if status != http.StatusAccepted {
		t.Fatalf("upload: got HTTP %d (%s), want 202", status, upload.Error)
	}
	if len(upload.Documents) != 1 {
		t.Fatalf("upload: %d documents queued, want 1", len(upload.Documents))
	}
	docID := upload.Documents[0].DocumentID
	t.Logf("uploaded document %s", docID)

	// The indexer processes the upload asynchronously (Tika + store).
	t.Eventually(20*time.Second, "document appears in KB detail", func() (bool, string) {
		var detail utils.KBDTO
		if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+c.kb.ID), nil, &detail); st != http.StatusOK {
			return false, fmt.Sprintf("detail returned HTTP %d", st)
		}
		for _, d := range detail.Documents {
			if d.DocumentID == docID {
				return true, ""
			}
		}
		return false, "document not yet listed"
	})

	if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodDelete,
		t.KBURL("/v1/knowledge-bases/"+c.kb.ID+"/documents/"+docID), nil, nil); st != http.StatusAccepted {
		t.Fatalf("delete document: got HTTP %d, want 202", st)
	}

	t.Eventually(20*time.Second, "document disappears from KB detail", func() (bool, string) {
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

func (c *kbDocumentDeleteAsyncCase) TearDown(t *utils.T) {
	t.DeleteKB(c.kb.ID)
}
