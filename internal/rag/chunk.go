// Package rag holds the retrieval-augmented-generation helpers shared by the
// indexer (which chunks documents before embedding) and the inference service
// (which formats retrieved chunks into prompt context).
package rag

import (
	"fmt"
	"strings"
)

// Default chunking parameters, in runes (characters). These are deliberately
// conservative defaults suitable for Titan embeddings.
const (
	DefaultChunkSize = 1000
	DefaultOverlap   = 150
)

// Bounds on caller-chosen chunking, in runes.
//
// MinChunkSize exists because a chunk shorter than this carries too little
// surrounding context to mean anything on its own — retrieval returns a
// fragment the model cannot place. MaxChunkSize sits well under Titan v2's
// 8192-token input limit (a rune is at most a token, usually less), so a
// legal chunk can never be one the embedder refuses.
const (
	MinChunkSize = 100
	MaxChunkSize = 8000
)

// ValidateChunking reports whether a caller's chunk size and overlap are
// usable, returning a message naming the problem.
//
// Overlap must be strictly less than size: at overlap == size the window
// never advances, which would be an infinite loop rather than a bad result.
// Chunk() defends itself against that by substituting a default, but silently
// changing what someone asked for is the wrong answer at an API boundary —
// here it is refused so they find out.
func ValidateChunking(size, overlap int) error {
	if size < MinChunkSize || size > MaxChunkSize {
		return fmt.Errorf("chunk_size must be between %d and %d", MinChunkSize, MaxChunkSize)
	}
	if overlap < 0 {
		return fmt.Errorf("chunk_overlap must not be negative")
	}
	if overlap >= size {
		return fmt.Errorf("chunk_overlap (%d) must be less than chunk_size (%d)", overlap, size)
	}
	return nil
}

// Chunk splits text into overlapping windows of up to size runes, advancing
// by (size - overlap) each step. It first normalises whitespace. Empty or
// whitespace-only input yields no chunks.
func Chunk(text string, size, overlap int) []string {
	if size <= 0 {
		size = DefaultChunkSize
	}
	if overlap < 0 || overlap >= size {
		overlap = DefaultOverlap
		if overlap >= size {
			overlap = size / 4
		}
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}
	step := size - overlap
	var chunks []string
	for start := 0; start < len(runes); start += step {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		piece := strings.TrimSpace(string(runes[start:end]))
		if piece != "" {
			chunks = append(chunks, piece)
		}
		if end == len(runes) {
			break
		}
	}
	return chunks
}

// FormatContext renders retrieved chunk texts into a block suitable for
// prepending to a system prompt. Returns the empty string when there are no
// chunks. It takes plain strings so this shared package stays decoupled from
// the services' repository types.
func FormatContext(texts []string) string {
	if len(texts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("You have access to the following retrieved context. Use it to answer the user's question when relevant, and do not fabricate information beyond it.\n\n")
	for i, text := range texts {
		fmt.Fprintf(&b, "[Context %d]\n%s\n\n", i+1, text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// File is a named document text passed to FormatFiles.
type File struct {
	Name string
	Text string
}

// FormatFiles renders whole context files into a block suitable for
// prepending to a system prompt. Returns the empty string when there are no
// files.
func FormatFiles(files []File) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("You have access to the following files. Use their contents to answer the user's question when relevant, and do not fabricate information beyond them.\n\n")
	for i, f := range files {
		name := f.Name
		if name == "" {
			name = fmt.Sprintf("file-%d", i+1)
		}
		fmt.Fprintf(&b, "[File: %s]\n%s\n\n", name, f.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}
