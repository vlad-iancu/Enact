package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// Config holds the environment-driven settings of the filesystem store.
type Config struct {
	// Root is the directory holding every stored file. It must be readable by
	// every service that serves files and writable by every service that
	// produces them — which on more than one host means a shared mount, and
	// past that means object storage instead.
	//
	// Deliberately without a static default. A deployment sets it to a path
	// every service mounts; unset, NewFS falls back to a temporary directory,
	// which is what makes a locally-run binary work without a privileged path
	// existing on the developer's machine.
	Root string `env:"WORKFLOW_FILES_ROOT"`
}

// FSScheme prefixes references written by the filesystem store.
const FSScheme = "fs"

// metaSuffix names the companion entry holding a file's metadata. A
// filesystem has nowhere to put a content type, so it is written beside the
// bytes; the suffix is outside the UUID alphabet, so it can never collide
// with a key.
const metaSuffix = ".meta"

// FS stores files in a directory tree mirroring their keys:
//
//	<root>/workflows/<id>/executions/<id>/files/<uuid>
//	<root>/workflows/<id>/files/<uuid>
//
// The layout is the key, which is what makes cleanup a directory removal and
// makes a stored reference legible in a log line.
//
// It is correct for local development and for a deployment on one host. The
// runner writes and enact-main reads, so both must see the same filesystem; a
// second runner on another machine needs a store that is not a local
// directory.
type FS struct {
	root string
}

// The filesystem store is the reference implementation of Store: if the two
// drift apart, this is where it is noticed rather than at a call site.
var _ Store = (*FS)(nil)

// NewFS returns a store rooted at cfg.Root, creating the directory if it does
// not exist. A root that cannot be created is a configuration error worth
// failing startup for: the alternative is a service that runs and then loses
// every file it is given.
func NewFS(cfg Config) (*FS, error) {
	configured := cfg.Root
	if configured == "" {
		// A development fallback, not a deployment default: it is writable
		// anywhere, and it is obviously temporary, which is the right
		// impression to leave on anyone who finds files in it.
		configured = filepath.Join(os.TempDir(), "enact-workflow-files")
	}
	root, err := filepath.Abs(configured)
	if err != nil {
		return nil, fmt.Errorf("files: resolve root %q: %w", configured, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("files: create root %q: %w", root, err)
	}
	return &FS{root: root}, nil
}

// Root reports the directory this store resolved to.
//
// Worth exposing purely so a service can say it at startup. The root is either
// configured or a temp-directory fallback, and three separate services have to
// agree on it — so "which directory am I actually using" is a question someone
// will need answered, and reading it out of a running process is otherwise
// impossible.
func (s *FS) Root() string { return s.root }

func (s *FS) Scheme() string { return FSScheme }

// Put writes r into loc under a fresh key.
//
// The write goes to a temporary name and is renamed into place, so a reader
// never sees a half-written file and a failed write leaves nothing behind.
func (s *FS) Put(ctx context.Context, loc Location, r io.Reader, meta Metadata) (File, error) {
	if !loc.Valid() {
		return File{}, fmt.Errorf("files: invalid location %+v", loc)
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}

	key := loc.prefix() + "/" + uuid.NewString()
	path, err := s.path(key)
	if err != nil {
		return File{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return File{}, fmt.Errorf("files: create directory for %s: %w", key, err)
	}

	size, err := s.writeFile(path, r)
	if err != nil {
		return File{}, err
	}

	meta = meta.normalize()
	meta.Size = size
	if err := s.writeMeta(path, meta); err != nil {
		// The bytes without their metadata is a file nothing can serve, so
		// the pair is removed rather than half-kept.
		_ = os.Remove(path)
		return File{}, err
	}

	return File{
		Ref:      FSScheme + ":" + key,
		Name:     meta.Name,
		MimeType: meta.MimeType,
		Size:     meta.Size,
	}, nil
}

// writeFile copies r to path, enforcing MaxFileSize, and returns the number of
// bytes written.
func (s *FS) writeFile(path string, r io.Reader) (int64, error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return 0, fmt.Errorf("files: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	// Both are no-ops once the file has been closed and renamed; they matter
	// on every path that does not get that far.
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()

	// One byte past the limit is read deliberately: a stream that stops
	// exactly at the limit is within it, and only a stream that has more to
	// give has exceeded it.
	size, err := io.Copy(temp, io.LimitReader(r, MaxFileSize+1))
	if err != nil {
		return 0, fmt.Errorf("files: write %s: %w", filepath.Base(path), err)
	}
	if size > MaxFileSize {
		return 0, fmt.Errorf("%w: the limit is %d bytes", ErrTooLarge, int64(MaxFileSize))
	}
	if err := temp.Close(); err != nil {
		return 0, fmt.Errorf("files: close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return 0, fmt.Errorf("files: store %s: %w", filepath.Base(path), err)
	}
	return size, nil
}

func (s *FS) writeMeta(path string, meta Metadata) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("files: marshal metadata: %w", err)
	}
	if err := os.WriteFile(path+metaSuffix, body, 0o644); err != nil {
		return fmt.Errorf("files: write metadata for %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (s *FS) Open(ctx context.Context, ref string) (io.ReadCloser, error) {
	path, err := s.pathOf(ref)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	if err != nil {
		return nil, fmt.Errorf("files: open %s: %w", ref, err)
	}
	return file, nil
}

func (s *FS) Stat(ctx context.Context, ref string) (Metadata, error) {
	path, err := s.pathOf(ref)
	if err != nil {
		return Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	body, err := os.ReadFile(path + metaSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("files: read metadata for %s: %w", ref, err)
	}
	var meta Metadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return Metadata{}, fmt.Errorf("files: decode metadata for %s: %w", ref, err)
	}
	return meta, nil
}

// Copy duplicates a file into dst. The copy gets its own key, so deleting
// either location leaves the other intact — which is the whole point of
// copying rather than sharing a reference.
func (s *FS) Copy(ctx context.Context, dst Location, ref string) (File, error) {
	meta, err := s.Stat(ctx, ref)
	if err != nil {
		return File{}, err
	}
	source, err := s.Open(ctx, ref)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = source.Close() }()

	return s.Put(ctx, dst, source, meta)
}

// DeleteAt removes a location's directory and everything in it.
func (s *FS) DeleteAt(ctx context.Context, loc Location) error {
	if !loc.Valid() {
		return fmt.Errorf("files: invalid location %+v", loc)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(loc.prefix())
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("files: delete files of %s: %w", loc.prefix(), err)
	}
	return nil
}

// DeleteWorkflow removes the workflow's whole subtree: retained files and
// every execution's, in one removal.
func (s *FS) DeleteWorkflow(ctx context.Context, workflowID string) error {
	if !validSegment(workflowID) {
		return fmt.Errorf("files: invalid workflow id %q", workflowID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(workflowPrefix(workflowID))
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("files: delete files of workflow %s: %w", workflowID, err)
	}
	return nil
}

// pathOf resolves a reference to an absolute path, rejecting anything this
// store did not write.
func (s *FS) pathOf(ref string) (string, error) {
	scheme, _, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	if scheme != FSScheme {
		return "", fmt.Errorf("%w: %q is not a filesystem reference", ErrBadReference, ref)
	}
	_, key, _ := strings.Cut(ref, ":")
	return s.path(key)
}

// path joins a validated key onto the root.
//
// The key has already been checked segment by segment, so this cannot escape
// the root; the check afterwards is a second lock on the same door, because
// the cost of being wrong here is reading arbitrary files off the host.
func (s *FS) path(key string) (string, error) {
	path := filepath.Join(s.root, filepath.FromSlash(key))
	if path != s.root && !strings.HasPrefix(path, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: key %q escapes the root", ErrBadReference, key)
	}
	return path, nil
}
