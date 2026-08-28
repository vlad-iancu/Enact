package rag

import "testing"

func TestValidateChunking(t *testing.T) {
	cases := []struct {
		name    string
		size    int
		overlap int
		wantErr bool
	}{
		{"defaults", DefaultChunkSize, DefaultOverlap, false},
		{"minimum size with no overlap", MinChunkSize, 0, false},
		{"maximum size", MaxChunkSize, 0, false},
		{"overlap one below size", 200, 199, false},

		{"size below minimum", MinChunkSize - 1, 0, true},
		{"size above maximum", MaxChunkSize + 1, 0, true},
		{"zero size", 0, 0, true},
		{"negative overlap", 1000, -1, true},
		// The important one: at overlap == size the window never advances,
		// so Chunk would loop forever were it not for its own fallback.
		{"overlap equal to size", 1000, 1000, true},
		{"overlap above size", 1000, 1001, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateChunking(tc.size, tc.overlap)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateChunking(%d, %d) = nil, want an error", tc.size, tc.overlap)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateChunking(%d, %d) = %v, want nil", tc.size, tc.overlap, err)
			}
		})
	}
}

// TestChunkHonoursSizeAndOverlap pins that the values callers now choose
// actually shape the output — the window advances by size-overlap, so a
// smaller overlap yields fewer chunks over the same text.
func TestChunkHonoursSizeAndOverlap(t *testing.T) {
	text := make([]rune, 1000)
	for i := range text {
		text[i] = 'a' + rune(i%26)
	}
	in := string(text)

	// size 200, overlap 0 -> step 200 -> 5 chunks of 200.
	got := Chunk(in, 200, 0)
	if len(got) != 5 {
		t.Errorf("Chunk(size=200, overlap=0) produced %d chunks, want 5", len(got))
	}
	for i, c := range got {
		if len([]rune(c)) != 200 {
			t.Errorf("chunk %d is %d runes, want 200", i, len([]rune(c)))
		}
	}

	// Same size, overlap 100 -> step 100 -> ~10 chunks: more overlap, more
	// chunks, which is the cost the parameter buys.
	overlapped := Chunk(in, 200, 100)
	if len(overlapped) <= len(got) {
		t.Errorf("overlap 100 produced %d chunks, want more than the %d from overlap 0",
			len(overlapped), len(got))
	}

	// The overlap is real: chunk 2 must begin with the tail of chunk 1.
	if len(overlapped) >= 2 {
		first := []rune(overlapped[0])
		second := []rune(overlapped[1])
		tail := string(first[len(first)-100:])
		if head := string(second[:100]); head != tail {
			t.Errorf("chunk 2 starts %q, want the 100-rune tail of chunk 1 %q", head, tail)
		}
	}
}
