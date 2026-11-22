package session

import (
	"errors"
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
	// A copy or archive path it cannot read or make is an error, not an empty archive.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "archive")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ copyDir, archiveDir, want string }{
		{filepath.Join(blocker, "copy"), archiveDir, "reading the transcript copy"},
		{copyDir, filepath.Join(blocker, "archive"), "checking the transcript archive"},
		{copyDir, link, "creating the transcript archive"},
	} {
		if err := archiveCopiedTranscripts(tc.copyDir, tc.archiveDir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want wording %q", tc.archiveDir, err, tc.want)
		}
	}
}
