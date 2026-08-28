package cases

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"enact/internal/enacttests/utils"
)

// kbChunkingParametersCase covers per-knowledge-base chunking: that a chosen
// size and overlap are recorded, that they actually reach the indexer, that
// bad values and misplaced ones are refused, and that there is no way to
// change them afterwards.
//
// The end-to-end half is what matters. A deliberately small chunk size over a
// fixed body of text must produce visibly more chunks than the platform
// default would — checked by comparing two knowledge bases fed the identical
// document, which is the only way to tell "the parameter was honoured" from
// "the parameter was stored and ignored".
type kbChunkingParametersCase struct {
	tuned    utils.KBDTO
	standard utils.KBDTO
	context  utils.KBDTO
}

func NewKBChunkingParameters() utils.TestCase { return &kbChunkingParametersCase{} }

func (c *kbChunkingParametersCase) Name() string { return "TestKB_ChunkingParameters" }

// chunkingText is ~1200 runes: more than one default chunk (1000) and many
// small ones, so the two knowledge bases below cannot come out equal.
var chunkingText = strings.Repeat("Sea otters hold hands while sleeping so they do not drift apart. ", 19)

func (c *kbChunkingParametersCase) Setup(t *utils.T) {
	c.tuned = c.createRAG(t, `"chunk_size":200,"chunk_overlap":20`)
	c.standard = c.createRAG(t, "")
	c.context = t.CreateKBOfKind("context")
}

// createRAG makes a retrieval KB, optionally with chunking fields.
func (c *kbChunkingParametersCase) createRAG(t *utils.T, extra string) utils.KBDTO {
	body := `{"name":"chunking test kb","kind":"rag"`
	if extra != "" {
		body += "," + extra
	}
	body += "}"
	var out utils.KBDTO
	if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodPost,
		t.KBURL("/v1/knowledge-bases"), strings.NewReader(body), &out); st != http.StatusCreated {
		t.Fatalf("create rag kb %s: got HTTP %d (%s), want 201", extra, st, out.Error)
	}
	return out
}

func (c *kbChunkingParametersCase) Run(t *utils.T) {
	// Chosen values are recorded; omitted ones are filled with the platform
	// defaults rather than left blank, so the record always says how its
	// documents will be split.
	if c.tuned.ChunkSize != 200 || c.tuned.ChunkOverlap != 20 {
		t.Errorf("tuned kb reports chunk_size=%d chunk_overlap=%d, want 200/20",
			c.tuned.ChunkSize, c.tuned.ChunkOverlap)
	}
	if c.standard.ChunkSize != 1000 || c.standard.ChunkOverlap != 150 {
		t.Errorf("default kb reports chunk_size=%d chunk_overlap=%d, want the platform defaults 1000/150",
			c.standard.ChunkSize, c.standard.ChunkOverlap)
	}
	// A context KB does not chunk, so it carries nothing.
	if c.context.ChunkSize != 0 || c.context.ChunkOverlap != 0 {
		t.Errorf("context kb reports chunk_size=%d chunk_overlap=%d, want neither",
			c.context.ChunkSize, c.context.ChunkOverlap)
	}

	c.rejects(t, "chunk_size below the minimum", `{"name":"x","kind":"rag","chunk_size":10}`)
	c.rejects(t, "chunk_size above the maximum", `{"name":"x","kind":"rag","chunk_size":99999}`)
	c.rejects(t, "overlap not below size", `{"name":"x","kind":"rag","chunk_size":300,"chunk_overlap":300}`)
	c.rejects(t, "negative overlap", `{"name":"x","kind":"rag","chunk_size":300,"chunk_overlap":-1}`)
	c.rejects(t, "chunking on a context kb", `{"name":"x","kind":"context","chunk_size":300}`)

	// Creation only: the update body has no such field, so the attempt is
	// refused outright rather than quietly accepted and ignored.
	var upd utils.KBDTO
	st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodPut,
		t.KBURL("/v1/knowledge-bases/"+c.tuned.ID), strings.NewReader(`{"chunk_size":400}`), &upd)
	if st != http.StatusBadRequest {
		t.Errorf("update chunk_size: got HTTP %d, want 400", st)
	}

	// And now the part that proves the value is used rather than merely
	// stored: the same document into both knowledge bases.
	tunedCount := c.uploadAndCountChunks(t, c.tuned.ID)
	standardCount := c.uploadAndCountChunks(t, c.standard.ID)
	t.Logf("chunks produced: tuned(200/20)=%d default(1000/150)=%d", tunedCount, standardCount)
	if tunedCount <= standardCount {
		t.Errorf("chunk_size=200 produced %d chunks and the 1000-rune default produced %d; "+
			"the smaller size must produce more, so the setting is not reaching the indexer",
			tunedCount, standardCount)
	}
}

// rejects asserts a create body is refused with a message.
func (c *kbChunkingParametersCase) rejects(t *utils.T, what, body string) {
	var out utils.KBDTO
	st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodPost,
		t.KBURL("/v1/knowledge-bases"), strings.NewReader(body), &out)
	if st != http.StatusBadRequest {
		t.Errorf("%s: got HTTP %d, want 400", what, st)
		if out.ID != "" {
			t.DeleteKB(out.ID)
		}
		return
	}
	if out.Error == "" {
		t.Errorf("%s: rejected with an empty error message", what)
	}
}

// uploadAndCountChunks puts one document into a KB and returns how many
// chunks it produced.
//
// Chunks are indexed one at a time, so a listing taken mid-flight reports a
// partial count — the first reading above zero is meaningless and comparing
// two of those is a coin toss. The count is therefore read repeatedly until
// it stops changing, which is the only "done" signal available from outside
// the indexer.
func (c *kbChunkingParametersCase) uploadAndCountChunks(t *utils.T, kbID string) int {
	var upload struct {
		Documents []struct {
			DocumentID string `json:"document_id"`
		} `json:"documents"`
		Error string `json:"error"`
	}
	st := t.DoMultipart("enact-tests", utils.KBAudience,
		t.KBURL("/v1/knowledge-bases/"+kbID+"/documents"),
		"chunking.txt", []byte(chunkingText), &upload)
	if st != http.StatusAccepted {
		t.Fatalf("upload to %s: got HTTP %d (%s), want 202", kbID, st, upload.Error)
	}
	docID := upload.Documents[0].DocumentID

	count, stable := 0, 0
	t.Eventually(60*time.Second, fmt.Sprintf("chunk count settles for %s", kbID), func() (bool, string) {
		var detail utils.KBDTO
		if s := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet,
			t.KBURL("/v1/knowledge-bases/"+kbID), nil, &detail); s != http.StatusOK {
			return false, fmt.Sprintf("detail returned HTTP %d", s)
		}
		found := 0
		for _, d := range detail.Documents {
			if d.DocumentID == docID {
				found = d.Chunks
				break
			}
		}
		if found == 0 {
			return false, "document not yet chunked"
		}
		if found == count {
			// Eventually polls every 250ms and the indexer writes a chunk
			// roughly every 350ms (a Bedrock embed call each), so a single
			// repeat proves nothing — it is the expected gap between two
			// writes. Four agreeing readings is a full second of quiet,
			// comfortably longer than one write takes.
			stable++
			return stable >= 4, fmt.Sprintf("count %d seen %d times", found, stable+1)
		}
		count, stable = found, 0
		return false, fmt.Sprintf("count still climbing: %d", found)
	})
	return count
}

func (c *kbChunkingParametersCase) TearDown(t *utils.T) {
	t.DeleteKB(c.tuned.ID)
	t.DeleteKB(c.standard.ID)
	t.DeleteKB(c.context.ID)
}
