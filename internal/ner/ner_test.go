package ner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tinyVocab writes a WordPiece vocabulary big enough to exercise the splitter
// without the 109 MB model, so this package's tests run anywhere.
func tinyVocab(t *testing.T) *Vocabulary {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "vocab.txt")
	tokens := []string{
		"[PAD]", "[UNK]", "[CLS]", "[SEP]",
		"Open", "##Search", "Elastic", "##search", "the", "cluster", "runs", ".",
		"g", "##RP", "##C", "6",
	}
	if err := os.WriteFile(path, []byte(strings.Join(tokens, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := LoadVocabulary(path)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestTokenizeSplitsLikeBERT covers the piece the model is most sensitive to:
// get WordPiece wrong and every prediction is made on the wrong input, quietly.
func TestTokenizeSplitsLikeBERT(t *testing.T) {
	v := tinyVocab(t)

	pieces := v.Tokenize("OpenSearch cluster runs", 128)
	var texts []string
	for _, p := range pieces {
		texts = append(texts, p.Text)
	}
	want := []string{"Open", "##Search", "cluster", "runs"}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("pieces = %v, want %v", texts, want)
	}
	// Both pieces of a split word must point at the same word, or a prediction
	// on "##Search" cannot be attributed back to "OpenSearch".
	if pieces[0].Word != pieces[1].Word {
		t.Errorf("word indices %d and %d differ across pieces of one word",
			pieces[0].Word, pieces[1].Word)
	}
	if pieces[2].Word == pieces[1].Word {
		t.Error("a following word shares the index of the previous one")
	}
}

// TestTokenizeIsCaseSensitive guards the model's single most important
// feature. Lowercasing here would be invisible and would destroy it.
func TestTokenizeIsCaseSensitive(t *testing.T) {
	v := tinyVocab(t)
	upper := v.Tokenize("OpenSearch", 128)
	lower := v.Tokenize("opensearch", 128)
	if len(upper) == 0 || upper[0].Text != "Open" {
		t.Fatalf("OpenSearch tokenised as %v", upper)
	}
	if len(lower) != 1 || lower[0].Text != tokenUNK {
		t.Errorf("opensearch tokenised as %v, want a single [UNK] — the cased vocabulary "+
			"has no lowercase entry, and pretending otherwise hides the model's real limit", lower)
	}
}

// TestTokenizeUnknownWordIsOneToken keeps a garbage token from looking like
// several pieces of evidence.
func TestTokenizeUnknownWordIsOneToken(t *testing.T) {
	v := tinyVocab(t)
	pieces := v.Tokenize("zzzqqq", 128)
	if len(pieces) != 1 || pieces[0].Text != tokenUNK {
		t.Errorf("got %v, want a single [UNK]", pieces)
	}
}

// TestSplitWordsSeparatesPunctuation matches BERT's basic tokenizer, which the
// model was trained behind: "gRPC/HTTP" is three words to it, not one.
func TestSplitWordsSeparatesPunctuation(t *testing.T) {
	got := Words("gRPC/HTTP, IPv6.")
	want := []string{"gRPC", "/", "HTTP", ",", "IPv6", "."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Words() = %v, want %v", got, want)
	}
}

// TestTokenizeRespectsTheWindow leaves room for [CLS] and [SEP], which the
// caller adds — overrunning would silently truncate the sentence the model
// sees.
func TestTokenizeRespectsTheWindow(t *testing.T) {
	v := tinyVocab(t)
	long := strings.Repeat("cluster runs ", 200)
	pieces := v.Tokenize(long, 32)
	if len(pieces) > 30 {
		t.Errorf("got %d pieces for a window of 32; two slots must be left for [CLS] and [SEP]",
			len(pieces))
	}
}

// TestDisabledIsANoOp is the property that lets this be wired in
// unconditionally: the tree's only native dependency must never be required.
func TestDisabledIsANoOp(t *testing.T) {
	model, err := New(Config{Enabled: false, ModelDir: "/nonexistent"})
	if err != nil {
		t.Fatalf("New with Enabled=false returned %v; it must not touch the filesystem", err)
	}
	if model != nil {
		t.Error("New returned a model despite being disabled")
	}
}

// TestMissingModelIsAnError, and a helpful one: the artefacts are fetched by a
// make target nobody will guess the name of.
func TestMissingModelIsAnError(t *testing.T) {
	_, err := New(Config{Enabled: true, ModelDir: t.TempDir()})
	if err == nil {
		t.Fatal("New succeeded with no model present")
	}
	if !strings.Contains(err.Error(), "make ner-model") {
		t.Errorf("error %q does not say how to fix it", err)
	}
}
