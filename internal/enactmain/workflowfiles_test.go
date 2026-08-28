package enactmain

import (
	"strings"
	"testing"
)

// A stored filename is whatever a third-party API called the document, so it
// reaches this header as untrusted text. Anything that could end the header
// and start another one has to be gone by the time it is written.
func TestContentDispositionCannotInjectAHeader(t *testing.T) {
	for _, tc := range []struct {
		what string
		name string
	}{
		{"a carriage return and newline", "q3.pdf\r\nSet-Cookie: session=stolen"},
		{"a bare newline", "q3\n.pdf"},
		{"a quote closing the value", `q3".pdf`},
		{"a backslash escaping the quote", `q3\".pdf`},
		{"a null byte", "q3\x00.pdf"},
		{"a delete character", "q3\x7f.pdf"},
	} {
		got := contentDisposition(tc.name)
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Errorf("%s: %q survived into %q", tc.what, tc.name, got)
		}
		// One quoted value: an unescaped quote inside it would make two.
		if strings.Count(got, `"`) != 2 {
			t.Errorf("%s: %q produced %q, which is not one quoted value", tc.what, tc.name, got)
		}
		if !strings.HasPrefix(got, "attachment; ") {
			t.Errorf("%s: %q is not offered as an attachment", tc.what, got)
		}
	}
}

func TestContentDispositionKeepsAnOrdinaryName(t *testing.T) {
	got := contentDisposition("q3-report (final).pdf")
	if !strings.Contains(got, `filename="q3-report (final).pdf"`) {
		t.Errorf("an ordinary name was mangled: %q", got)
	}
	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("%q carries no RFC 5987 form", got)
	}
}

// Non-ASCII cannot go in the quoted form, but the client should still be
// offered the real name.
func TestContentDispositionCarriesNonASCIINames(t *testing.T) {
	got := contentDisposition("rapport-trimestriel-é.pdf")
	if strings.Contains(got, "é") && !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("%q puts a non-ASCII character in the quoted form", got)
	}
	if !strings.Contains(got, "%C3%A9") {
		t.Errorf("%q does not carry the encoded name", got)
	}
}

// A file with no name is ordinary: the store does not require one.
func TestContentDispositionWithoutAName(t *testing.T) {
	if got := contentDisposition(""); got != "attachment" {
		t.Errorf("contentDisposition(\"\") = %q, want %q", got, "attachment")
	}
}
