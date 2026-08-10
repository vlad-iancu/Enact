package s2s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "material.yaml")
	if err := os.WriteFile(path, []byte("keys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("reads file when content empty", func(t *testing.T) {
		got, err := ResolveContent("", path, "S2S_JWKS_FILE")
		if err != nil {
			t.Fatal(err)
		}
		if got != "keys: []\n" {
			t.Errorf("got %q, want file content", got)
		}
	})

	t.Run("content wins over file", func(t *testing.T) {
		got, err := ResolveContent("inline", path, "S2S_JWKS_FILE")
		if err != nil {
			t.Fatal(err)
		}
		if got != "inline" {
			t.Errorf("got %q, want inline content", got)
		}
	})

	t.Run("both empty is empty", func(t *testing.T) {
		got, err := ResolveContent("", "", "S2S_JWKS_FILE")
		if err != nil || got != "" {
			t.Errorf("got (%q, %v), want empty and no error", got, err)
		}
	})

	t.Run("missing file names the variable", func(t *testing.T) {
		_, err := ResolveContent("", filepath.Join(dir, "absent"), "S2S_ACL_FILE")
		if err == nil || !strings.Contains(err.Error(), "S2S_ACL_FILE") {
			t.Errorf("err = %v, want mention of S2S_ACL_FILE", err)
		}
	})
}
