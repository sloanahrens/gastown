package gitfake

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFakeAddedFiles pins AddedFiles and BlobBytes to the real behavior: a file
// added then deleted in the range is listed and readable, one already on base is
// not, a path and content added by two commits is listed once, and bad input is
// an error.
func TestFakeAddedFiles(t *testing.T) {
	t.Parallel()
	f, h := addedFilesWorld(t)
	t.Run("deleted in range listed and readable, base not, repeated pair once", func(t *testing.T) {
		t.Parallel()
		files, err := h.AddedFiles("base", "HEAD")
		if err != nil {
			t.Fatalf("AddedFiles: %v", err)
		}
		if len(files) != 2 || files[0].Path != "dup.txt" || files[1].Path != "gone.txt" ||
			len(files[0].Blob) != 40 || len(files[1].Blob) != 40 {
			t.Fatalf("AddedFiles = %v, want dup.txt and gone.txt once each, both with a 40-hex blob", files)
		}
		content, err := h.BlobBytes(files[1].Blob)
		if err != nil {
			t.Fatalf("BlobBytes(gone.txt): %v", err)
		}
		if string(content) != "gone bytes\n" {
			t.Errorf("BlobBytes(gone.txt) = %q, want %q", content, "gone bytes\n")
		}
	})
	t.Run("bad input is an error", func(t *testing.T) {
		t.Parallel()
		if files, err := h.AddedFiles("nope", "HEAD"); err == nil || files != nil {
			t.Errorf("AddedFiles(unknown revision) = %v, %v; want an error and no list", files, err)
		}
		if _, err := h.BlobBytes("HEAD"); err == nil || !strings.Contains(err.Error(), "full hex") {
			t.Errorf("BlobBytes(HEAD) = %v, want the full-hex refusal", err)
		}
		if content, err := h.BlobBytes(strings.Repeat("a", 40)); err == nil || content != nil {
			t.Errorf("BlobBytes(unknown blob) = %q, %v; want an error and no bytes", content, err)
		}
		dir := t.TempDir()
		if content, err := f.Open(dir).(*handle).BlobBytes(blobID("on base\n")); err == nil || content != nil {
			t.Errorf("BlobBytes outside a repository = %q, %v; want an error", content, err)
		}
	})
}

// addedFilesWorld builds a repository where base..main adds gone.txt and then
// deletes it, and adds dup.txt, removes it and adds it again with the same
// content, over a base holding base.txt.
func addedFilesWorld(t *testing.T) (*Fake, *handle) {
	t.Helper()
	f := New()
	dir := filepath.Join(t.TempDir(), "repo")
	f.InitBare(t, dir)
	base := f.Commit(t, dir, "main", "base", map[string]string{"base.txt": "on base\n"})
	f.SetRef(t, dir, "refs/heads/base", base)
	f.Commit(t, dir, "main", "add", map[string]string{"gone.txt": "gone bytes\n", "dup.txt": "same\n"})
	f.CommitRemoving(t, dir, "main", "drop", nil, "gone.txt", "dup.txt")
	f.Commit(t, dir, "main", "re-add", map[string]string{"dup.txt": "same\n"})
	return f, f.Open(dir).(*handle)
}
