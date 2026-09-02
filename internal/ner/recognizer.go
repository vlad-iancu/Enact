package ner

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// Config points at the artefacts and bounds the work.
//
// All of it is optional: with Enabled false — the default — nothing is loaded,
// no shared library is touched, and the crawler behaves exactly as it did
// before. That matters more than usual here, because this is the first native
// dependency in the tree and a deployment that cannot satisfy it must still
// run.
type Config struct {
	Enabled bool `env:"NER_ENABLED, default=false"`
	// ModelDir holds model_int8.onnx and vocab.txt. `make ner-model` fetches
	// them; they are far too large to keep in the repository.
	ModelDir string `env:"NER_MODEL_DIR, default=dist/models/bert-base-NER"`
	// LibraryPath is the ONNX Runtime shared library. Empty lets the binding
	// look in the usual places.
	LibraryPath string `env:"NER_ONNXRUNTIME_PATH, default=dist/onnxruntime/libonnxruntime.dylib"`
	// MaxTokens caps one inference. BERT's own limit is 512; long pages are
	// split into windows of this size rather than truncated, so a name late in
	// a document is still found.
	MaxTokens int `env:"NER_MAX_TOKENS, default=256"`
	// MinScore is the softmax probability a label needs. The model is
	// confident when it is right and diffuse when it is guessing, so this
	// removes most of the noise for one comparison per token.
	MinScore float64 `env:"NER_MIN_SCORE, default=0.6"`
	// Threads bounds ONNX Runtime's own pool. One is right here: the crawler
	// scores pages sequentially, and letting the runtime spawn a pool per
	// session on a machine also running eight services is worse than useless.
	Threads int `env:"NER_THREADS, default=1"`
}

// Recognizer finds names in text.
//
// An interface at the point of use so internal/wsd never imports ONNX, CGO or
// this package: the analysis pipeline asks a question and something answers
// it, or nothing does.
type Recognizer interface {
	// Names returns the distinct names found in text, lowercased.
	Names(text string) map[string]bool
}

// Model is an ONNX token classifier.
type Model struct {
	vocab   *Vocabulary
	session *ort.DynamicAdvancedSession
	labels  []string
	cfg     Config

	// mu serialises inference. ONNX Runtime sessions are not safe for
	// concurrent Run calls without per-call IO binding, and the crawler has no
	// need for it — pages are scored one at a time.
	mu sync.Mutex
}

// initOnce guards the process-wide ONNX Runtime initialisation, which the
// binding requires exactly once and which is not reference counted.
var initOnce sync.Once
var initErr error

// New loads the model. It returns (nil, nil) when NER is disabled, so callers
// can wire it in unconditionally.
func New(cfg Config) (*Model, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.MaxTokens <= 0 || cfg.MaxTokens > 512 {
		cfg.MaxTokens = 256
	}
	if cfg.Threads <= 0 {
		cfg.Threads = 1
	}

	modelPath := filepath.Join(cfg.ModelDir, "model_int8.onnx")
	vocabPath := filepath.Join(cfg.ModelDir, "vocab.txt")
	for _, path := range []string{modelPath, vocabPath} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("ner: %s is missing; run `make ner-model`: %w", path, err)
		}
	}

	initOnce.Do(func() {
		if cfg.LibraryPath != "" {
			if _, err := os.Stat(cfg.LibraryPath); err == nil {
				ort.SetSharedLibraryPath(cfg.LibraryPath)
			}
		}
		initErr = ort.InitializeEnvironment()
	})
	if initErr != nil {
		return nil, fmt.Errorf("ner: initialise onnxruntime (see NER_ONNXRUNTIME_PATH): %w", initErr)
	}

	vocab, err := LoadVocabulary(vocabPath)
	if err != nil {
		return nil, fmt.Errorf("ner: load vocabulary: %w", err)
	}

	options, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer options.Destroy()
	if err := options.SetIntraOpNumThreads(cfg.Threads); err != nil {
		return nil, err
	}
	if err := options.SetInterOpNumThreads(cfg.Threads); err != nil {
		return nil, err
	}

	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"logits"}, options)
	if err != nil {
		return nil, fmt.Errorf("ner: open %s: %w", modelPath, err)
	}

	m := &Model{
		vocab: vocab, session: session, cfg: cfg,
		// CoNLL-2003, in the order the config file gives them.
		labels: []string{"O", "B-MISC", "I-MISC", "B-PER", "I-PER",
			"B-ORG", "I-ORG", "B-LOC", "I-LOC"},
	}
	// The session holds C memory that the garbage collector knows nothing
	// about, and the model outlives every request, so tie its release to the
	// object rather than to a Close nobody would call.
	runtime.SetFinalizer(m, func(m *Model) { m.session.Destroy() })
	return m, nil
}

// Names returns the distinct names in text, lowercased.
//
// Lowercased because the caller matches them against lemmas, and the entire
// point is to recognise a name written one way and mark it somewhere it is
// written another.
func (m *Model) Names(text string) map[string]bool {
	names := make(map[string]bool)
	if m == nil || strings.TrimSpace(text) == "" {
		return names
	}
	words := Words(text)
	pieces := m.vocab.Tokenize(text, m.cfg.MaxTokens)
	if len(pieces) == 0 {
		return names
	}

	// A page is far longer than the model's window, so it is read in windows.
	// Splitting on word boundaries keeps every window a real piece of prose,
	// which is what the model was trained on.
	for start := 0; start < len(pieces); {
		end := start + m.cfg.MaxTokens - 2
		if end > len(pieces) {
			end = len(pieces)
		}
		// Do not cut a word in half between windows.
		for end < len(pieces) && end > start && pieces[end].Word == pieces[end-1].Word {
			end--
		}
		if end == start {
			break
		}
		m.markWindow(pieces[start:end], words, names)
		start = end
	}
	return names
}

// markWindow runs one inference and records the words it labelled.
func (m *Model) markWindow(pieces []Piece, words []string, names map[string]bool) {
	n := len(pieces) + 2
	ids := make([]int64, n)
	mask := make([]int64, n)
	types := make([]int64, n)
	ids[0] = m.vocab.ids[tokenCLS]
	for i, piece := range pieces {
		ids[i+1] = piece.ID
	}
	ids[n-1] = m.vocab.ids[tokenSEP]
	for i := range mask {
		mask[i] = 1
	}

	shape := ort.NewShape(1, int64(n))
	inputIDs, err := ort.NewTensor(shape, ids)
	if err != nil {
		return
	}
	defer inputIDs.Destroy()
	attention, err := ort.NewTensor(shape, mask)
	if err != nil {
		return
	}
	defer attention.Destroy()
	tokenTypes, err := ort.NewTensor(shape, types)
	if err != nil {
		return
	}
	defer tokenTypes.Destroy()

	logits := []ort.Value{nil}
	m.mu.Lock()
	err = m.session.Run([]ort.Value{inputIDs, attention, tokenTypes}, logits)
	m.mu.Unlock()
	if err != nil || logits[0] == nil {
		return
	}
	defer logits[0].Destroy()

	out, ok := logits[0].(*ort.Tensor[float32])
	if !ok {
		return
	}
	data := out.GetData()
	classes := len(m.labels)
	if len(data) < n*classes {
		return
	}

	// Attribute a piece's label to the word it came from. The first piece of a
	// word carries the decision: BERT labels "Open" and lets "##Search"
	// continue it, so reading every piece would double-count and reading the
	// last would miss the B- tag entirely.
	decided := map[int]bool{}
	for i, piece := range pieces {
		if decided[piece.Word] {
			continue
		}
		label, score := best(data[(i+1)*classes:(i+2)*classes], m.labels)
		decided[piece.Word] = true
		if label == "O" || score < m.cfg.MinScore {
			continue
		}
		if piece.Word < len(words) {
			word := strings.ToLower(strings.Trim(words[piece.Word], ".,;:!?()[]{}\"'"))
			if len(word) > 1 {
				names[word] = true
			}
		}
	}
}

// best is argmax plus the softmax probability of the winner.
func best(row []float32, labels []string) (string, float64) {
	if len(row) < len(labels) {
		return "O", 0
	}
	top, max := 0, float64(row[0])
	for i := 1; i < len(labels); i++ {
		if float64(row[i]) > max {
			top, max = i, float64(row[i])
		}
	}
	var sum float64
	for i := range labels {
		sum += math.Exp(float64(row[i]) - max)
	}
	if sum == 0 {
		return labels[top], 0
	}
	return labels[top], 1 / sum
}
