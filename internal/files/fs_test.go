package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *FS {
	t.Helper()
	store, err := NewFS(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("create the store: %v", err)
	}
	return store
}

func put(t *testing.T, store *FS, loc Location, body string, meta Metadata) File {
	t.Helper()
	file, err := store.Put(context.Background(), loc, strings.NewReader(body), meta)
	if err != nil {
		t.Fatalf("store a file: %v", err)
	}
	return file
}

func read(t *testing.T, store *FS, ref string) string {
	t.Helper()
	reader, err := store.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("open %s: %v", ref, err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read %s: %v", ref, err)
	}
	return string(body)
}

func TestPutThenOpenReturnsWhatWasStored(t *testing.T) {
	store := newTestStore(t)
	loc := InExecution("w1", "e9")

	file := put(t, store, loc, "hello", Metadata{Name: "greeting.txt", MimeType: "text/plain"})

	if got, want := read(t, store, file.Ref), "hello"; got != want {
		t.Errorf("read back %q, want %q", got, want)
	}
	if file.Size != 5 {
		t.Errorf("size = %d, want 5", file.Size)
	}
	// The reference must be resolvable back to where it belongs, since that
	// is what the API authorizes against.
	scheme, parsed, err := ParseRef(file.Ref)
	if err != nil {
		t.Fatalf("the store wrote a reference it cannot parse: %v", err)
	}
	if scheme != FSScheme || parsed != loc {
		t.Errorf("reference %q resolves to %s/%+v, want fs/%+v", file.Ref, scheme, parsed, loc)
	}
}

func TestPutAssignsDistinctKeys(t *testing.T) {
	store := newTestStore(t)
	loc := InExecution("w1", "e9")

	first := put(t, store, loc, "one", Metadata{MimeType: "text/plain"})
	second := put(t, store, loc, "two", Metadata{MimeType: "text/plain"})

	if first.Ref == second.Ref {
		t.Fatal("two files at one location were given the same reference")
	}
	if got := read(t, store, first.Ref); got != "one" {
		t.Errorf("the first file reads back as %q", got)
	}
}

func TestPutDefaultsAnUnlabelledType(t *testing.T) {
	store := newTestStore(t)
	file := put(t, store, InWorkflow("w1"), "bytes", Metadata{})
	if file.MimeType != DefaultMimeType {
		t.Errorf("mime type = %q, want %q", file.MimeType, DefaultMimeType)
	}
}

func TestPutRefusesAnOversizedFileAndLeavesNothingBehind(t *testing.T) {
	store := newTestStore(t)
	loc := InExecution("w1", "e9")

	// A reader that never ends: the limit has to stop this, not the source.
	endless := io.LimitReader(neverEnding{}, MaxFileSize+1024)
	_, err := store.Put(context.Background(), loc, endless, Metadata{MimeType: "application/octet-stream"})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}

	// The partial write must not survive, or a failed upload would still
	// consume the disk it was refused for.
	dir := filepath.Join(store.root, filepath.FromSlash(loc.prefix()))
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the location directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused write left %d entries behind", len(entries))
	}
}

func TestPutAcceptsAFileExactlyAtTheLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 100 MiB")
	}
	store := newTestStore(t)
	atLimit := io.LimitReader(neverEnding{}, MaxFileSize)

	file, err := store.Put(context.Background(), InWorkflow("w1"), atLimit, Metadata{MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("a file exactly at the limit was refused: %v", err)
	}
	if file.Size != MaxFileSize {
		t.Errorf("size = %d, want %d", file.Size, int64(MaxFileSize))
	}
}

func TestStatReportsStoredMetadata(t *testing.T) {
	store := newTestStore(t)
	file := put(t, store, InWorkflow("w1"), "report", Metadata{Name: "q3.pdf", MimeType: "application/pdf"})

	meta, err := store.Stat(context.Background(), file.Ref)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if meta.Name != "q3.pdf" || meta.MimeType != "application/pdf" || meta.Size != 6 {
		t.Errorf("metadata = %+v, want q3.pdf/application/pdf/6", meta)
	}
}

// A retained file is the reason Copy exists: the run's own files go when the
// execution does, so anything meant to be kept is copied out first.
func TestCopyToARetainedLocationOutlivesTheExecution(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	execution := InExecution("w1", "e9")

	original := put(t, store, execution, "keep me", Metadata{Name: "out.txt", MimeType: "text/plain"})
	kept, err := store.Copy(ctx, InWorkflow("w1"), original.Ref)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if kept.Ref == original.Ref {
		t.Fatal("the copy reuses the original's reference")
	}
	if kept.Name != "out.txt" || kept.MimeType != "text/plain" {
		t.Errorf("the copy lost its metadata: %+v", kept)
	}

	if err := store.DeleteAt(ctx, execution); err != nil {
		t.Fatalf("delete the execution's files: %v", err)
	}
	if got := read(t, store, kept.Ref); got != "keep me" {
		t.Errorf("the retained copy reads back as %q", got)
	}
	if _, err := store.Open(ctx, original.Ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("opening the deleted original returned %v, want ErrNotFound", err)
	}
}

func TestDeleteAtRemovesOnlyItsOwnLocation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	doomed := put(t, store, InExecution("w1", "e9"), "gone", Metadata{MimeType: "text/plain"})
	sibling := put(t, store, InExecution("w1", "e10"), "still here", Metadata{MimeType: "text/plain"})

	if err := store.DeleteAt(ctx, InExecution("w1", "e9")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Open(ctx, doomed.Ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted file opened with %v, want ErrNotFound", err)
	}
	if got := read(t, store, sibling.Ref); got != "still here" {
		t.Errorf("another execution's file reads back as %q", got)
	}
}

func TestDeleteAtIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	// Deleting an execution that produced no files at all is ordinary: it is
	// what cleanup does after a run that failed on its first step.
	if err := store.DeleteAt(context.Background(), InExecution("w1", "never-ran")); err != nil {
		t.Errorf("deleting an empty location failed: %v", err)
	}
}

func TestOpenRejectsReferencesTheStoreDidNotWrite(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// A file outside the tree, to prove traversal cannot reach it.
	outside := filepath.Join(filepath.Dir(store.root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write the decoy: %v", err)
	}

	for _, tc := range []struct {
		what string
		ref  string
	}{
		{"another scheme", "s3:workflows/w1/files/2f1c"},
		{"no scheme", "workflows/w1/files/2f1c"},
		{"traversal out of the root", "fs:workflows/w1/files/../../../outside.txt"},
		{"absolute path", "fs:/etc/passwd"},
	} {
		if _, err := store.Open(ctx, tc.ref); !errors.Is(err, ErrBadReference) {
			t.Errorf("%s: opening %q returned %v, want ErrBadReference", tc.what, tc.ref, err)
		}
	}
}

func TestOpenAndStatReportAMissingFile(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	missing := "fs:workflows/w1/files/" + strings.Repeat("a", 8)

	if _, err := store.Open(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("open returned %v, want ErrNotFound", err)
	}
	if _, err := store.Stat(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat returned %v, want ErrNotFound", err)
	}
}

func TestPutRefusesAnInvalidLocation(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Put(context.Background(), InExecution("", "e9"), strings.NewReader("x"), Metadata{})
	if err == nil {
		t.Error("a location with no workflow id was accepted")
	}
}

// neverEnding is an infinite source of zero bytes, for testing the size limit
// without holding the limit in memory.
type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// An unset root must still produce a usable store: services run as local
// binaries in development, where a privileged path does not exist.
func TestNewFSFallsBackToATemporaryDirectory(t *testing.T) {
	store, err := NewFS(Config{})
	if err != nil {
		t.Fatalf("an unset root failed: %v", err)
	}
	if !strings.HasPrefix(store.root, os.TempDir()) {
		t.Errorf("root = %q, want a path under %q", store.root, os.TempDir())
	}
}

// Deleting a workflow must reach BOTH kinds of file. Retained files are the
// point of the distinction, so a cascade that removed only the executions'
// would leak exactly the files someone chose to keep.
func TestDeleteWorkflowRemovesRetainedAndExecutionFiles(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	retained := put(t, store, InWorkflow("w1"), "kept", Metadata{MimeType: "text/plain"})
	firstRun := put(t, store, InExecution("w1", "e9"), "run one", Metadata{MimeType: "text/plain"})
	secondRun := put(t, store, InExecution("w1", "e10"), "run two", Metadata{MimeType: "text/plain"})
	otherWorkflow := put(t, store, InExecution("w2", "e1"), "elsewhere", Metadata{MimeType: "text/plain"})

	if err := store.DeleteWorkflow(ctx, "w1"); err != nil {
		t.Fatalf("delete the workflow's files: %v", err)
	}

	for _, ref := range []string{retained.Ref, firstRun.Ref, secondRun.Ref} {
		if _, err := store.Open(ctx, ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s survived the cascade (%v)", ref, err)
		}
	}
	if got := read(t, store, otherWorkflow.Ref); got != "elsewhere" {
		t.Errorf("another workflow's file reads back as %q", got)
	}
}

func TestDeleteWorkflowIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	// A workflow that never produced a file is the common case, and deleting
	// one is ordinary rather than an error.
	if err := store.DeleteWorkflow(context.Background(), "never-ran"); err != nil {
		t.Errorf("deleting a workflow with no files failed: %v", err)
	}
}

func TestDeleteWorkflowRefusesAnUnusableID(t *testing.T) {
	store := newTestStore(t)
	for _, id := range []string{"", "..", "w1/e9", "../../etc"} {
		if err := store.DeleteWorkflow(context.Background(), id); err == nil {
			t.Errorf("workflow id %q was accepted", id)
		}
	}
}
