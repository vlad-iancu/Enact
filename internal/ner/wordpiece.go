// Package ner recognises names in text with a transformer token classifier
// run through ONNX Runtime.
//
// It exists because every case-based signal the crawler had — the proper-noun
// tag, capitalisation, prose's own extractor — is really the same signal wearing
// three hats, and all three fail together on the text that matters. Measured:
// "OpenSearch" tags NNP in five contexts out of five, "opensearch" in none of
// them, and prose's extractor finds nothing in a lowercase query at all.
//
// The model is a BERT token classifier fine-tuned on CoNLL-2003
// (PER/ORG/LOC/MISC), quantised to int8. It is cased, so it does not read
// lowercase names either — but that is not what it is for here. Pages are
// well-capitalised prose, and this reads them far better than prose does; the
// names it finds there are then carried to the lowercase query by
// wsd.CollectNames. Improving the page side is what improves the query side.
//
// Everything is optional. A deployment with no model file, no shared library,
// or NER_ENABLED unset runs exactly as it did before, because the recogniser
// is injected as an interface and a nil one is a no-op.
package ner

import (
	"bufio"
	"os"
	"strings"
	"unicode"
)

// Special tokens every BERT vocabulary defines.
const (
	tokenCLS = "[CLS]"
	tokenSEP = "[SEP]"
	tokenUNK = "[UNK]"
	tokenPAD = "[PAD]"
)

// Vocabulary is a WordPiece vocabulary: the token strings the model was
// trained on, and their ids.
type Vocabulary struct {
	ids map[string]int64
	// maxToken bounds the greedy longest-match scan below, so a pathological
	// run of letters cannot make tokenisation quadratic in its own length.
	maxToken int
}

// LoadVocabulary reads a vocab.txt — one token per line, id is the line number.
func LoadVocabulary(path string) (*Vocabulary, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	v := &Vocabulary{ids: make(map[string]int64, 30000)}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var id int64
	for scanner.Scan() {
		token := strings.TrimRight(scanner.Text(), "\r\n")
		if _, seen := v.ids[token]; !seen {
			v.ids[token] = id
		}
		if n := len(token); n > v.maxToken {
			v.maxToken = n
		}
		id++
	}
	return v, scanner.Err()
}

// Size is how many tokens the vocabulary holds.
func (v *Vocabulary) Size() int { return len(v.ids) }

// Piece is one WordPiece token and the word it came from.
//
// Word is the index of the whitespace/punctuation-delimited word this piece
// belongs to, which is what lets a prediction made on "##Search" be attributed
// back to "OpenSearch" — the model labels pieces, the caller wants words.
type Piece struct {
	ID   int64
	Text string
	Word int
}

// Tokenize splits text the way BERT does: whitespace and punctuation first,
// then greedy longest-match WordPiece within each word.
//
// Cased, deliberately — the model is cased, and lowercasing here would destroy
// the one feature it relies on most.
func (v *Vocabulary) Tokenize(text string, maxLen int) []Piece {
	words := splitWords(text)
	pieces := make([]Piece, 0, len(words)+2)
	// Room for [CLS] and [SEP].
	budget := maxLen - 2
	for index, word := range words {
		if len(pieces) >= budget {
			break
		}
		for _, piece := range v.wordPieces(word, index) {
			if len(pieces) >= budget {
				break
			}
			pieces = append(pieces, piece)
		}
	}
	return pieces
}

// wordPieces splits one word, longest match first, with continuations marked
// "##". A word no prefix of which is in the vocabulary becomes a single [UNK].
func (v *Vocabulary) wordPieces(word string, index int) []Piece {
	runes := []rune(word)
	var out []Piece
	start := 0
	for start < len(runes) {
		end := len(runes)
		if limit := start + v.maxToken; limit < end {
			end = limit
		}
		var found string
		var id int64
		for ; end > start; end-- {
			candidate := string(runes[start:end])
			if start > 0 {
				candidate = "##" + candidate
			}
			if tokenID, ok := v.ids[candidate]; ok {
				found, id = candidate, tokenID
				break
			}
		}
		if found == "" {
			// Nothing matched: the whole word is unknown. Emitting one [UNK]
			// rather than a per-character fallback matches the reference
			// implementation, and keeps a garbage token from looking like
			// several pieces of evidence.
			return []Piece{{ID: v.ids[tokenUNK], Text: tokenUNK, Word: index}}
		}
		out = append(out, Piece{ID: id, Text: found, Word: index})
		start = end
	}
	return out
}

// splitWords separates on whitespace and punctuation, keeping punctuation as
// its own word — BERT's basic tokenizer, minus the accent stripping that a
// cased English model does not do.
func splitWords(text string) []string {
	var words []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isPunct(r):
			flush()
			words = append(words, string(r))
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return words
}

// isPunct follows BERT in treating every ASCII symbol as punctuation, not only
// the ones Unicode classifies that way — so "gRPC/HTTP" splits but "cohere-768"
// splits too, which is what the model saw in training.
func isPunct(r rune) bool {
	if (r >= '!' && r <= '/') || (r >= ':' && r <= '@') ||
		(r >= '[' && r <= '`') || (r >= '{' && r <= '~') {
		return true
	}
	return unicode.IsPunct(r)
}

// Words re-splits text the same way Tokenize does, so a caller can map word
// indices back to the strings they came from.
func Words(text string) []string { return splitWords(text) }
