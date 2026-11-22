package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
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

// brokenDirFS is an fs.FS whose directory "bad" cannot be read.
type brokenDirFS struct{ fs.FS }

func (b brokenDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "bad" {
		return nil, errors.New("directory vanished")
	}
	return fs.ReadDir(b.FS, name)
}

// TestTranscriptArchiveListReportsAnUnreadableDirectory: a subdirectory that
// cannot be read is reported naming it, and the transcripts beside it are still
// listed (gt-1xoiib).
func TestTranscriptArchiveListReportsAnUnreadableDirectory(t *testing.T) {
	t.Parallel()
	fsys := brokenDirFS{fstest.MapFS{
		"ses-1.jsonl":       {Data: []byte("x")},
		"bad/agent-1.jsonl": {Data: []byte("x")},
		"note.txt":          {Data: []byte("x")},
	}}
	found, errs := listTranscripts(fsys)
	if len(found) != 1 || found[0] != "ses-1.jsonl" {
		t.Errorf("listed %v, want only ses-1.jsonl", found)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "reading the transcript copy bad") {
		t.Errorf("errors = %v, want one naming the directory bad", errs)
	}
}

// archiveFixture is a copy holding one transcript and the paths a move uses.
func archiveFixture(t *testing.T) (root *os.Root, own, dest, copyDir, archiveDir string) {
	t.Helper()
	base := t.TempDir()
	copyDir, archiveDir = filepath.Join(base, "copy"), filepath.Join(base, "archive")
	own = filepath.Join(copyDir, "ses-1.jsonl")
	writeTranscript(t, own)
	return mustRoot(t, copyDir), own, filepath.Join(archiveDir, "ses-1.jsonl"), copyDir, archiveDir
}

// TestTranscriptArchiveEveryFailedStepKeepsTheSource: each step of a move can
// fail; the error names the file and the step, the source stays in the copy,
// no .partial is left, and a truncated final name never exists. A failure after
// the link leaves a complete archived file beside the kept source (gt-1xoiib).
func TestTranscriptArchiveEveryFailedStepKeepsTheSource(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"open", "fstat", "mkdir", "create", "copy", "close", "chtimes", "link", "unpartial", "remove"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			root, own, dest, _, _ := archiveFixture(t)
			injected := errors.New("injected " + step)
			err := moveConfinedWith(root, "ses-1.jsonl", dest, func(s string, _ *os.File) error {
				if s == step {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) || !strings.Contains(err.Error(), "ses-1.jsonl") || !strings.Contains(err.Error(), "("+step+")") {
				t.Fatalf("err = %v, want the %s failure naming the file and the step", err, step)
			}
			if _, serr := os.Stat(own); serr != nil {
				t.Errorf("the source left the copy after a failed %s: %v", step, serr)
			}
			if _, serr := os.Stat(dest + ".partial"); !errors.Is(serr, fs.ErrNotExist) {
				t.Errorf("a .partial is left after a failed %s: %v", step, serr)
			}
			_, derr := os.Stat(dest)
			afterLink := step == "unpartial" || step == "remove"
			if afterLink && derr != nil {
				t.Errorf("after the link the archived file must be complete and present: %v", derr)
			}
			if !afterLink && !errors.Is(derr, fs.ErrNotExist) {
				t.Errorf("a failed %s left a final-name file: %v", step, derr)
			}
		})
	}
}

// TestTranscriptArchiveRefusesALeafSwappedAfterTheLstat: the copy is written by
// an untrusted sandbox, so the leaf can change between the Lstat and the open. A
// FIFO must not hang the open and a link must not be read under its own name;
// each is an error and the source stays (gt-1xoiib).
func TestTranscriptArchiveRefusesALeafSwappedAfterTheLstat(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"fifo", "link"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			root, own, dest, copyDir, _ := archiveFixture(t)
			other := filepath.Join(copyDir, "other.jsonl")
			writeTranscript(t, other)
			swap := func(step string, _ *os.File) error {
				if step != "open" {
					return nil
				}
				if err := os.Remove(own); err != nil {
					return err
				}
				if kind == "fifo" {
					return syscall.Mkfifo(own, 0o600)
				}
				return os.Symlink(filepath.Base(other), own)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- moveConfinedWith(root, "ses-1.jsonl", dest, swap) }()
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal("the move hung on a leaf swapped for a " + kind)
			}
			if err == nil || !strings.Contains(err.Error(), "ses-1.jsonl") {
				t.Fatalf("err = %v, want the swapped %s refused naming the file", err, kind)
			}
			if _, serr := os.Lstat(own); serr != nil {
				t.Errorf("the swapped leaf left the copy: %v", serr)
			}
			if _, serr := os.Stat(dest); !errors.Is(serr, fs.ErrNotExist) {
				t.Errorf("the swapped %s reached the archive: %v", kind, serr)
			}
		})
	}
}

// TestTranscriptArchiveARepeatedNameIsAnError: two files of one base name in
// different folders are an error naming both, the second stays in the copy, and
// the first is archived; a name the archive already held stays silently
// "already archived" (gt-1xoiib).
func TestTranscriptArchiveARepeatedNameIsAnError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	copyDir, archiveDir := filepath.Join(root, "copy"), filepath.Join(root, "archive")
	first, second := filepath.Join(copyDir, "a", "agent-1.jsonl"), filepath.Join(copyDir, "b", "agent-1.jsonl")
	writeTranscript(t, first)
	writeTranscript(t, second)
	err := archiveCopiedTranscripts(copyDir, archiveDir)
	if err == nil || !strings.Contains(err.Error(), "a/agent-1.jsonl") || !strings.Contains(err.Error(), "b/agent-1.jsonl") {
		t.Fatalf("err = %v, want an error naming both paths", err)
	}
	if _, serr := os.Stat(second); serr != nil {
		t.Errorf("the second file left the copy: %v", serr)
	}
	if got, serr := os.ReadFile(filepath.Join(archiveDir, "agent-1.jsonl")); serr != nil || string(got) != first {
		t.Errorf("archived = %q, %v; want the first file's content", got, serr)
	}
}

// TestTranscriptArchiveClearsALeftoverPartial: a kill mid-copy leaves a
// .partial; the next run overwrites it and archives the whole file, and no
// truncated final name exists in between (gt-1xoiib).
func TestTranscriptArchiveClearsALeftoverPartial(t *testing.T) {
	t.Parallel()
	_, own, dest, copyDir, archiveDir := archiveFixture(t)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".partial", []byte("trunc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveCopiedTranscripts(copyDir, archiveDir); err != nil {
		t.Fatalf("archiveCopiedTranscripts: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != own {
		t.Errorf("archived = %q, %v; want the whole source", got, err)
	}
	if _, err := os.Stat(dest + ".partial"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the leftover partial is still there: %v", err)
	}
}

// TestTranscriptArchiveRealStepFailuresKeepTheSource: the steps that read the
// leaf fail for real states the hook arranges before them: a leaf gone before
// the Lstat, a handle closed before its Stat, a leaf gone between the open and
// the second Lstat. Each is an error naming the step, and nothing is archived
// (gt-1xoiib).
func TestTranscriptArchiveRealStepFailuresKeepTheSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, at string
		arrange  func(own string, src *os.File)
	}{
		{"lstat", "lstat", func(own string, _ *os.File) { _ = os.Remove(own) }},
		{"fstat on a closed handle", "fstat", func(_ string, src *os.File) { _ = src.Close() }},
		{"second lstat", "fstat", func(own string, _ *os.File) { _ = os.Remove(own) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root, own, dest, _, _ := archiveFixture(t)
			err := moveConfinedWith(root, "ses-1.jsonl", dest, func(step string, src *os.File) error {
				if step == c.at {
					c.arrange(own, src)
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "ses-1.jsonl") {
				t.Fatalf("err = %v, want a failure naming the file", err)
			}
			if _, serr := os.Stat(dest); !errors.Is(serr, fs.ErrNotExist) {
				t.Errorf("a failed move reached the archive: %v", serr)
			}
		})
	}
}

// TestTranscriptArchiveAPartialItCannotClearFailsTheMove: a leftover .partial
// that cannot be removed (here a directory with a file in it) fails the move at
// the create step naming it, and the source stays (gt-1xoiib).
func TestTranscriptArchiveAPartialItCannotClearFailsTheMove(t *testing.T) {
	t.Parallel()
	root, own, dest, _, _ := archiveFixture(t)
	if err := os.MkdirAll(dest+".partial", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest+".partial", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := moveConfined(root, "ses-1.jsonl", dest)
	if err == nil || !strings.Contains(err.Error(), "(create)") {
		t.Fatalf("err = %v, want the create step to fail", err)
	}
	if _, serr := os.Stat(own); serr != nil {
		t.Errorf("the source left the copy: %v", serr)
	}
}
