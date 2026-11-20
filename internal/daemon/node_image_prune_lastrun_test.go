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

// A run id of the real update shape round-trips. The stamp holds a capital T
// and Z, so an id check that drops the capital-letter clause refuses every real
// run id and the prune is never recorded.
func TestNodeImagePruneLastRealRunID(t *testing.T) {
	t.Parallel()

	const runID = "20261104T200648Z-e41a9ded"
	town := t.TempDir()
	if err := writeNodeImagePruneLast(town, runID); err != nil {
		t.Fatalf("writeNodeImagePruneLast(%q) error = %v", runID, err)
	}
	if id, err := readNodeImagePruneLast(town); err != nil || id != runID {
		t.Fatalf("readNodeImagePruneLast() = %q, %v, want %q", id, err, runID)
	}
}

// A write over an existing record replaces it at mode 0600 and leaves no other
// file behind: os.WriteFile keeps the old 0644 mode, and the temp file the
// atomic write renames must be gone.
func TestNodeImagePruneLastReplacesAnExistingRecord(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	runtimeDir := filepath.Join(town, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	path := filepath.Join(runtimeDir, nodeImagePruneLastFile)
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil { // outrun a narrow umask
		t.Fatalf("Chmod() error = %v", err)
	}
	const newID = "20261104T200648Z-e41a9ded"
	if err := writeNodeImagePruneLast(town, newID); err != nil {
		t.Fatalf("writeNodeImagePruneLast() error = %v", err)
	}
	if id, err := readNodeImagePruneLast(town); err != nil || id != newID {
		t.Fatalf("readNodeImagePruneLast() = %q, %v, want %q", id, err, newID)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("file mode = %o, want 600", got)
	}
	if entries, err := os.ReadDir(runtimeDir); err != nil || len(entries) != 1 {
		t.Errorf("runtime directory entries = %v, %v, want only %q", entries, err, nodeImagePruneLastFile)
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
