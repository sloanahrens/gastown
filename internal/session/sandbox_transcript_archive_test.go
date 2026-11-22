package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// archiveAge is the modification time the archive tests give a transcript, so
// what a move keeps never depends on the wall clock.
var archiveAge = time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

// writeTranscript writes a transcript at path with path as its body, last
// written at archiveAge, so a test can tell which file a move kept.
func writeTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, archiveAge, archiveAge); err != nil {
		t.Fatal(err)
	}
}

// TestTranscriptArchiveMovesTheCopy: the copy's own and subagent transcripts
// move flat into the archive with their mtimes and out of the copy; a name the
// archive already holds is left in the copy; a missing copy archives nothing;
// and a copy or archive it cannot read or make is an error.
func TestTranscriptArchiveMovesTheCopy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	copyDir, archiveDir := filepath.Join(root, "copy"), filepath.Join(root, "archive")
	own, sub := filepath.Join(copyDir, "ses-1.jsonl"), filepath.Join(copyDir, "subagents", "agent-1.jsonl")
	writeTranscript(t, own)
	writeTranscript(t, sub)
	writeTranscript(t, filepath.Join(archiveDir, "ses-9.jsonl"))
	if err := archiveCopiedTranscripts(copyDir, archiveDir); err != nil {
		t.Fatalf("archiveCopiedTranscripts: %v", err)
	}
	for _, p := range []string{own, sub} {
		dest := filepath.Join(archiveDir, filepath.Base(p))
		if info, err := os.Stat(dest); err != nil || !info.ModTime().Equal(archiveAge) {
			t.Errorf("archived %s: %v, %v; want it flat with its mtime", dest, info, err)
		}
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still in the copy: %v", p, err)
		}
	}
	// A name the archive already holds is kept there and left in the copy.
	writeTranscript(t, filepath.Join(copyDir, "ses-9.jsonl"))
	if err := archiveCopiedTranscripts(copyDir, archiveDir); err != nil {
		t.Fatalf("archiveCopiedTranscripts with an archived name: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(archiveDir, "ses-9.jsonl")); err != nil || string(got) != filepath.Join(archiveDir, "ses-9.jsonl") {
		t.Errorf("archived ses-9.jsonl = %q, %v; want the one already archived", got, err)
	}
	if _, err := os.Stat(filepath.Join(copyDir, "ses-9.jsonl")); err != nil {
		t.Errorf("the skipped transcript left the copy: %v", err)
	}
	// The copy comes from an untrusted sandbox: a link named like a transcript is
	// not a regular file, so it stays in the copy and never enters the archive.
	outside := filepath.Join(root, "outside.jsonl")
	writeTranscript(t, outside)
	linked := filepath.Join(copyDir, "linked.jsonl")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	if err := archiveCopiedTranscripts(copyDir, archiveDir); err != nil {
		t.Fatalf("archiveCopiedTranscripts with a link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(archiveDir, "linked.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a link entered the archive: %v", err)
	}
	if _, err := os.Lstat(linked); err != nil {
		t.Errorf("the link left the copy: %v", err)
	}
	// A missing copy archives nothing and makes no archive.
	if err := archiveCopiedTranscripts(filepath.Join(root, "missing"), filepath.Join(root, "missing-archive")); err != nil {
		t.Fatalf("archiveCopiedTranscripts on a missing copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "missing-archive")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing copy made an archive: %v", err)
	}
	// A copy path it cannot open, or an archive it cannot make, is an error, not
	// an empty archive; the files that can move still do.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ copyDir, archiveDir, want string }{
		{filepath.Join(blocker, "copy"), archiveDir, "opening the transcript copy"},
		{copyDir, filepath.Join(blocker, "archive"), "into the transcript archive"},
	} {
		writeTranscript(t, filepath.Join(copyDir, "fresh.jsonl"))
		if err := archiveCopiedTranscripts(tc.copyDir, tc.archiveDir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want wording %q", tc.archiveDir, err, tc.want)
		}
	}
}

// TestTranscriptArchiveRenameFailureNamesTheFile: a move that fails is an error
// naming the file, and the failed file stays in the copy (gt-1xoiib).
func TestTranscriptArchiveRenameFailureNamesTheFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	copyDir, archiveDir := filepath.Join(root, "copy"), filepath.Join(root, "archive")
	own := filepath.Join(copyDir, "ses-1.jsonl")
	writeTranscript(t, own)
	failed := errors.New("rename refused")
	err := archiveCopiedTranscriptsWith(copyDir, archiveDir, func(_ *os.Root, rel, _ string) error {
		return fmt.Errorf("moving %s into the transcript archive: %w", rel, failed)
	})
	if !errors.Is(err, failed) || !strings.Contains(err.Error(), "ses-1.jsonl") {
		t.Fatalf("a failed move = %v, want the move's error naming %s", err, own)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("the file whose move failed left the copy: %v", err)
	}
}

// TestTranscriptArchiveRefusesAParentSwappedForALink: the copy comes from an
// untrusted sandbox. A subagent folder swapped for a link to a directory outside
// the copy, between the listing and the move, must not move the transcript it
// holds there: the move is refused with an error naming the path, the outside
// file stays, and nothing reaches the archive (gt-1xoiib).
func TestTranscriptArchiveRefusesAParentSwappedForALink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	copyDir, archiveDir, outside := filepath.Join(root, "copy"), filepath.Join(root, "archive"), filepath.Join(root, "outside")
	secret := filepath.Join(outside, "host.jsonl")
	writeTranscript(t, secret)
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(copyDir, "subagents")); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(archiveDir, "host.jsonl")
	err := moveConfined(mustRoot(t, copyDir), "subagents/host.jsonl", dest)
	if err == nil || !strings.Contains(err.Error(), "subagents/host.jsonl") {
		t.Errorf("a move through a link out of the copy = %v, want an error naming the path", err)
	}
	if _, serr := os.Stat(secret); serr != nil {
		t.Errorf("the file outside the copy moved: %v", serr)
	}
	if _, serr := os.Stat(dest); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("the outside file reached the archive: %v", serr)
	}
	if _, serr := os.Stat(archiveDir); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("the refused move made the archive directory: %v", serr)
	}
	// A leaf swapped for a link, even to a file inside the copy, is refused too.
	target := filepath.Join(copyDir, "real.jsonl")
	writeTranscript(t, target)
	if err := os.Symlink(target, filepath.Join(copyDir, "swapped.jsonl")); err != nil {
		t.Fatal(err)
	}
	err = moveConfined(mustRoot(t, copyDir), "swapped.jsonl", dest)
	if err == nil || !strings.Contains(err.Error(), "no longer a regular file") {
		t.Errorf("a move of a link leaf = %v, want it refused as no longer a regular file", err)
	}
	if _, serr := os.Stat(dest); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("the link's target reached the archive: %v", serr)
	}
}

func mustRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
