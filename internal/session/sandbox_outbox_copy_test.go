package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// outboxSeat is the record of the seat the outbox copy tests copy from.
func outboxSeat() SandboxRecord {
	return SandboxRecord{
		Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat, Gateway: "gw1",
	}
}

// outboxRemote is the sandbox path of one outbox file, as the listing names it.
func outboxRemote(name string) string {
	return sandboxOutboxDir + "/" + name
}

// outboxRunner is the CLI runner a test installs: the outbox listing answers
// with listing, and a download of a path in files writes that body into the
// directory it was given. Every call goes on to the fake, so its calls and
// failures are the fake's. A transcript listing reaches the fake unanswered,
// which is the seat having written no transcript.
func outboxRunner(fake *sandbox.Fake, listing string, files map[string]string) sandbox.Runner {
	return seatRunner(fake, listing, "", files)
}

// seatRunner is outboxRunner for the whole copy-out a seat's end makes: each
// listing answers with its own lines.
func seatRunner(fake *sandbox.Fake, outboxListing, transcriptListing string, files map[string]string) sandbox.Runner {
	run := fake.Runner()
	return func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		out, errOut, err := run(ctx, argv, stdin, env)
		switch {
		case hasArgText(argv, "find "+sandboxOutboxDir):
			return []byte(outboxListing), errOut, err
		case hasArg(argv, sandboxProjectsDir):
			return []byte(transcriptListing), errOut, err
		case hasArg(argv, "download"):
			if err != nil {
				return out, errOut, err
			}
			remote, localDir := argv[len(argv)-2], argv[len(argv)-1]
			if body, ok := files[remote]; ok {
				if err := os.MkdirAll(localDir, 0o755); err != nil {
					return nil, nil, err
				}
				if err := os.WriteFile(filepath.Join(localDir, path.Base(remote)), []byte(body), 0o644); err != nil {
					return nil, nil, err
				}
			}
		}
		return out, errOut, err
	}
}

// hasArg reports whether argv holds want.
func hasArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// hasArgText reports whether any element of argv holds text.
func hasArgText(argv []string, text string) bool {
	for _, a := range argv {
		if strings.Contains(a, text) {
			return true
		}
	}
	return false
}

// outboxCopyDir is the directory the seat's outbox is copied into.
func outboxCopyDir(b *openshellBackend) string {
	return SandboxOutboxCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)
}

// TestNoBeadAccessSeatEndCopiesTheOutboxFilesOut: both outbox files the sandbox
// holds land on the host, byte for byte, in the copy's own order, and the copy
// deletes nothing.
func TestNoBeadAccessSeatEndCopiesTheOutboxFilesOut(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	const result = `{"bead_id":"gt-1","status":"done"}`
	const notes = "# notes\n"
	b.cli.Run = outboxRunner(fake, "result.json 34\nnotes.md 8\n", map[string]string{
		outboxRemote(sandboxOutboxResult): result,
		outboxRemote(sandboxOutboxNotes):  notes,
	})

	if err := b.copySeatOutbox(outboxSeat()); err != nil {
		t.Fatalf("copySeatOutbox: %v", err)
	}
	dir := outboxCopyDir(b)
	for name, want := range map[string]string{sandboxOutboxResult: result, sandboxOutboxNotes: notes} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading the copied %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("copied %s = %q, want %q", name, got, want)
		}
	}
	calls := fake.Calls()
	want := []string{outboxRemote(sandboxOutboxResult), outboxRemote(sandboxOutboxNotes)}
	if got := downloadedRemotes(calls); !reflect.DeepEqual(got, want) {
		t.Errorf("downloaded %v, want %v", got, want)
	}
	if callIndex(calls, "delete") >= 0 {
		t.Error("the outbox copy deleted the sandbox")
	}
}

// TestNoBeadAccessSeatEndCopiesTheOutboxAndTranscriptBeforeTheRemove: one
// seat's whole copy-out, in the order the seat end makes it. Its outbox is
// copied first, then the delete copies its transcripts, and the sandbox is
// removed last, so both are on the host before it goes (gt-c6tofd).
func TestNoBeadAccessSeatEndCopiesTheOutboxAndTranscriptBeforeTheRemove(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	const result = `{"bead_id":"gt-1","status":"done"}`
	transcript := seatRemote("ses-1.jsonl")
	b.cli.Run = seatRunner(fake, "result.json 34\n", listingLine(transcript, seatBase, 4), map[string]string{
		outboxRemote(sandboxOutboxResult): result,
		transcript:                        `{"type":"assistant"}`,
	})
	writeOpsRecord(t, b)

	if err := b.copySeatOutbox(outboxSeat()); err != nil {
		t.Fatalf("copySeatOutbox: %v", err)
	}
	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	calls := fake.Calls()
	outbox, transcriptCopy, remove := -1, -1, callIndex(calls, "delete")
	for i, c := range calls {
		switch {
		case hasArg(c, "download") && c[len(c)-2] == outboxRemote(sandboxOutboxResult):
			outbox = i
		case hasArg(c, "download") && c[len(c)-2] == transcript:
			transcriptCopy = i
		}
	}
	if outbox < 0 || transcriptCopy < 0 || remove < 0 {
		t.Fatalf("openshell calls = %v, want the outbox copied, the transcript copied and the sandbox removed", calls)
	}
	if !(outbox < transcriptCopy && transcriptCopy < remove) {
		t.Errorf("outbox copy at %d, transcript copy at %d, sandbox removed at %d; want the outbox, then the transcript, then the remove", outbox, transcriptCopy, remove)
	}
	if _, err := os.Stat(filepath.Join(outboxCopyDir(b), sandboxOutboxResult)); err != nil {
		t.Errorf("the outbox is not on the host after the seat's copy-out: %v", err)
	}
	copied, _, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil || len(copied) != 1 {
		t.Errorf("copied transcripts = %v, err = %v, want ses-1.jsonl", copied, err)
	}
}

// TestNoBeadAccessSeatEndCopyWithNoOutboxIsNothingToCopy: a sandbox whose
// outbox directory is not there holds no outbox, which is not a failure and
// leaves no copy behind.
func TestNoBeadAccessSeatEndCopyWithNoOutboxIsNothingToCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = outboxRunner(fake, "", nil)

	if err := b.copySeatOutbox(outboxSeat()); err != nil {
		t.Fatalf("copySeatOutbox = %v, want nothing to copy", err)
	}
	if got := downloadedRemotes(fake.Calls()); len(got) != 0 {
		t.Errorf("downloaded %v, want nothing", got)
	}
	if _, err := os.Stat(outboxCopyDir(b)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of the copy = %v, want no copy", err)
	}
}

// TestNoBeadAccessSeatEndCopyClearsThePreviousSeatsOutbox: the copy is this
// seat's outbox or nothing, so a file a previous seat of the same name left is
// gone once this seat's outbox is copied.
func TestNoBeadAccessSeatEndCopyClearsThePreviousSeatsOutbox(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	dir := outboxCopyDir(b)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{sandboxOutboxResult, sandboxOutboxNotes} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("a previous seat's"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const result = `{"bead_id":"gt-1","status":"done"}`
	b.cli.Run = outboxRunner(fake, "result.json 34\n", map[string]string{outboxRemote(sandboxOutboxResult): result})

	if err := b.copySeatOutbox(outboxSeat()); err != nil {
		t.Fatalf("copySeatOutbox: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, sandboxOutboxResult)); err != nil || string(got) != result {
		t.Errorf("copied %s = %q, err = %v, want this seat's %q", sandboxOutboxResult, got, err, result)
	}
	if _, err := os.Stat(filepath.Join(dir, sandboxOutboxNotes)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of the previous seat's notes.md = %v, want it cleared", err)
	}
}

// TestNoBeadAccessSeatEndCopyRefusals: a listing the host cannot take at its
// word is refused, and nothing is downloaded. The sandbox writes this listing,
// so an entry it does not name, a name twice, a size that is not a byte count
// and a file past the copy's cap are each an error, never a shorter copy.
func TestNoBeadAccessSeatEndCopyRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		listing string
	}{
		{name: "an entry that is not an outbox file", listing: "other.json 5\n"},
		{name: "a line with no size", listing: sandboxOutboxResult + "\n"},
		{name: "a size that is not a byte count", listing: sandboxOutboxResult + " many\n"},
		{name: "a negative size", listing: sandboxOutboxResult + " -1\n"},
		{name: "a name listed twice", listing: "notes.md 1\nnotes.md 2\n"},
		{name: "a file past the copy's cap", listing: fmt.Sprintf("notes.md %d\n", sandboxOutboxCopyCap+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &sandbox.Fake{}
			b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
			b.cli.Run = outboxRunner(fake, tt.listing, map[string]string{
				outboxRemote(sandboxOutboxResult): "x",
				outboxRemote(sandboxOutboxNotes):  "x",
			})

			if err := b.copySeatOutbox(outboxSeat()); err == nil {
				t.Fatal("copySeatOutbox accepted the listing")
			}
			if got := downloadedRemotes(fake.Calls()); len(got) != 0 {
				t.Errorf("downloaded %v, want nothing", got)
			}
			if _, err := os.Stat(outboxCopyDir(b)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stat of the copy = %v, want no copy", err)
			}
		})
	}
}

// TestNoBeadAccessSeatEndCopyOfACopyDirItCannotPrepare: a copy directory the
// host cannot clear or create fails the copy, so a caller keeps a sandbox whose
// outbox it has nowhere to put.
func TestNoBeadAccessSeatEndCopyOfACopyDirItCannotPrepare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
	}{
		{
			// The parent of the copy directory is a file, so clearing the copy
			// fails rather than finding nothing there.
			name: "a copy path whose parent is not a directory",
			prepare: func(t *testing.T, dir string) {
				parent := filepath.Dir(dir)
				if err := os.MkdirAll(filepath.Dir(parent), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(parent, []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The parent of the copy directory takes no new entry, so creating
			// the copy fails.
			name: "a copy directory the host cannot create",
			prepare: func(t *testing.T, dir string) {
				parent := filepath.Dir(dir)
				if err := os.MkdirAll(parent, 0o755); err != nil {
					t.Fatal(err)
				}
				//testpolicy:allow no-exec-files — a directory's exec bits are its traverse permission, not an executable file
				if err := os.Chmod(parent, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					//testpolicy:allow no-exec-files — restoring the directory's traverse permission, not an executable file
					_ = os.Chmod(parent, 0o755)
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &sandbox.Fake{}
			b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
			b.cli.Run = outboxRunner(fake, "result.json 34\n", map[string]string{outboxRemote(sandboxOutboxResult): "x"})
			tt.prepare(t, outboxCopyDir(b))

			if err := b.copySeatOutbox(outboxSeat()); err == nil {
				t.Fatal("copySeatOutbox reported a copy it could not write")
			}
			if got := downloadedRemotes(fake.Calls()); len(got) != 0 {
				t.Errorf("downloaded %v, want nothing", got)
			}
		})
	}
}

// TestNoBeadAccessSeatEndCopyDownloadThatLeftNoRegularFile: a download that
// left a directory where the outbox file belongs, or a path the host cannot
// read, fails the copy rather than passing a reader something that is not the
// seat's outbox.
func TestNoBeadAccessSeatEndCopyDownloadThatLeftNoRegularFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		after func(localDir string) error
	}{
		{
			name: "a directory where the file belongs",
			after: func(localDir string) error {
				return os.MkdirAll(filepath.Join(localDir, sandboxOutboxResult), 0o755)
			},
		},
		{
			// The download's directory is replaced by a file, so reading what it
			// wrote fails instead of reporting no file.
			name: "a copy the host cannot read",
			after: func(localDir string) error {
				if err := os.RemoveAll(localDir); err != nil {
					return err
				}
				return os.WriteFile(localDir, []byte("not a directory"), 0o644)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &sandbox.Fake{}
			b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
			run := fake.Runner()
			b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
				out, errOut, err := run(ctx, argv, stdin, env)
				switch {
				case hasArg(argv, "download"):
					if err == nil {
						if afterErr := tt.after(argv[len(argv)-1]); afterErr != nil {
							return nil, nil, afterErr
						}
					}
				case hasArgText(argv, "find "+sandboxOutboxDir):
					return []byte("result.json 34\n"), errOut, err
				}
				return out, errOut, err
			}

			if err := b.copySeatOutbox(outboxSeat()); err == nil {
				t.Fatal("copySeatOutbox accepted a download that left no regular file")
			}
		})
	}
}

// TestNoBeadAccessSeatEndCopyFailedListingLeavesThePreviousCopy: a listing that
// cannot be read is an error, not an empty outbox, and it does not destroy the
// copy that was already there.
func TestNoBeadAccessSeatEndCopyFailedListingLeavesThePreviousCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "connection reset"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	dir := outboxCopyDir(b)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sandboxOutboxResult), []byte("copied earlier"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.copySeatOutbox(outboxSeat()); err == nil {
		t.Fatal("copySeatOutbox read an unreadable listing as an empty outbox")
	}
	if _, err := os.Stat(filepath.Join(dir, sandboxOutboxResult)); err != nil {
		t.Errorf("the copy already there was destroyed: %v", err)
	}
}

// TestNoBeadAccessSeatEndCopyFailedDownloadIsAnError: a failed download fails
// the copy and leaves no partial copy behind, so a reader never takes a part of
// the seat's outbox for the whole of it.
func TestNoBeadAccessSeatEndCopyFailedDownloadIsAnError(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"download": errors.New("exit status 1")},
		Stderr: map[string]string{"download": "connection reset"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = outboxRunner(fake, "result.json 34\nnotes.md 8\n", map[string]string{
		outboxRemote(sandboxOutboxResult): "x",
		outboxRemote(sandboxOutboxNotes):  "x",
	})

	if err := b.copySeatOutbox(outboxSeat()); err == nil {
		t.Fatal("copySeatOutbox accepted a failed download")
	}
	if _, err := os.Stat(outboxCopyDir(b)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of the copy = %v, want the partial copy removed", err)
	}
}

// TestNoBeadAccessSeatEndCopyDownloadThatWritesNoFileIsAnError: a download that
// wrote nothing where it was asked to fails the copy, rather than leaving a
// reader with an outbox that is not there.
func TestNoBeadAccessSeatEndCopyDownloadThatWritesNoFileIsAnError(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = outboxRunner(fake, "result.json 34\n", nil)

	if err := b.copySeatOutbox(outboxSeat()); err == nil {
		t.Fatal("copySeatOutbox accepted a download that wrote no file")
	}
	if _, err := os.Stat(outboxCopyDir(b)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of the copy = %v, want the partial copy removed", err)
	}
}

// TestNoBeadAccessSeatEndCopyOfAGoneSandboxIsNothingToCopy: a sandbox that is
// already gone has no outbox to copy, which is not a failure.
func TestNoBeadAccessSeatEndCopyOfAGoneSandboxIsNothingToCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "Error: sandbox not found: " + openshellTestSession},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))

	if err := b.copySeatOutbox(outboxSeat()); err != nil {
		t.Fatalf("copySeatOutbox = %v, want nothing to copy from a gone sandbox", err)
	}
	if _, err := os.Stat(outboxCopyDir(b)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of the copy = %v, want no copy", err)
	}
}

// TestNoBeadAccessSeatEndCopyRecordThatNamesNoSeatIsRefused: a record whose rig
// or polecat is not a plain name has no place to copy to, so the copy refuses
// rather than read a sandbox nothing can name. It drives the exported entry
// point a caller outside this package uses, which answers before any openshell
// call and so needs no gateway.
func TestNoBeadAccessSeatEndCopyRecordThatNamesNoSeatIsRefused(t *testing.T) {
	t.Parallel()
	rec := outboxSeat()
	rec.Polecat = "../" + rec.Polecat
	if err := CopySandboxSeatOutbox(t.TempDir(), rec); err == nil {
		t.Fatal("CopySandboxSeatOutbox copied for a record that names no seat")
	}
}
