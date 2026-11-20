package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A town with no last-prune file has pruned no run yet: the id is empty and
// the read is not an error.
func TestNodeImagePruneLastMissingFileReadsEmpty(t *testing.T) {
	t.Parallel()

	if id, err := readNodeImagePruneLast(t.TempDir()); err != nil || id != "" {
		t.Fatalf("readNodeImagePruneLast() = %q, %v, want the empty id and no error", id, err)
	}
}

// A written id reads back, and the file holds it at mode 0600.
func TestNodeImagePruneLastRoundTrip(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	if err := writeNodeImagePruneLast(town, "run-1_ok"); err != nil {
		t.Fatalf("writeNodeImagePruneLast() error = %v", err)
	}
	if id, err := readNodeImagePruneLast(town); err != nil || id != "run-1_ok" {
		t.Fatalf("readNodeImagePruneLast() = %q, %v, want %q", id, err, "run-1_ok")
	}
	info, err := os.Stat(filepath.Join(town, ".runtime", nodeImagePruneLastFile))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("file mode = %o, want 600", got)
	}
}

// A run id outside letters, digits, dash and underscore, or the empty id, is
// refused on write and leaves no file behind.
func TestNodeImagePruneLastRefusesBadID(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, id string }{
		{"empty", ""},
		{"slash", "run/1"},
		{"space", "run 1"},
		{"dot", "run.1"},
		{"newline", "run\n"},
		{"non-ascii", "runé"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			if err := writeNodeImagePruneLast(town, tt.id); err == nil {
				t.Fatalf("writeNodeImagePruneLast(%q) error = nil, want a refusal", tt.id)
			}
			if _, err := os.Stat(filepath.Join(town, ".runtime", nodeImagePruneLastFile)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a refused write of %q left the file: Stat() error = %v", tt.id, err)
			}
		})
	}
}

// A write that cannot create the runtime directory is an error, not a silent
// no-op: a prune recorded there did not happen.
func TestNodeImagePruneLastRefusesUnwritableRuntimeDir(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := writeNodeImagePruneLast(town, "run-1"); err == nil {
		t.Fatal("writeNodeImagePruneLast() error = nil, want an error")
	}
}

// A read that fails for a reason other than the file being absent is an error,
// not an empty id: an unreadable record is not an unpruned node.
func TestNodeImagePruneLastUnreadableIsAnError(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".runtime", nodeImagePruneLastFile), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if _, err := readNodeImagePruneLast(town); err == nil {
		t.Fatal("readNodeImagePruneLast() error = nil, want an error for an unreadable record")
	}
}
