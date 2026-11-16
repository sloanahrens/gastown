package cmd

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// A file-mode seat is a sandbox whose runtime archive staged the file-mode
// marker beside its task (gt-3qkaws). These tests give a beadVerbs a working
// directory inside such a seat instead of a sandbox, so every step from the
// verb to the outbox file runs.

// newFileModeSeat returns the root of a seat in file mode whose TASK.md names
// beadID, with the runtime archive's marker and task file written under it.
func newFileModeSeat(t *testing.T, beadID string) string {
	t.Helper()
	root := t.TempDir()
	taskDir := filepath.Join(root, seatTaskDir)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, taskfile.ModeFile), []byte(taskfile.ModeFiles+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, seatTaskFile), []byte("# TASK "+beadID+": a task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// seatVerbs returns verbs acting on root as a sandbox seat, and the store they
// would write a bead through. The verbs run from a worktree inside the seat,
// as a seat's commands do, so the walk from a working directory to the seat
// root is part of what every test here drives.
func seatVerbs(t *testing.T, root string) (*beadVerbs, *callingStore, *bytes.Buffer) {
	t.Helper()
	worktree := filepath.Join(root, "gt", "gastown", "polecats", "slit", "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	store := &callingStore{Client: beadsfake.New()}
	var out bytes.Buffer
	return &beadVerbs{
		client:  store,
		actor:   "gastown/polecats/slit",
		out:     &out,
		seatDir: func() string { return worktree },
	}, store, &out
}

// callingStore counts the calls a file-mode verb must not make: any AppendNotes
// it records is a bead write that escaped the outbox.
type callingStore struct {
	beads.Client
	calls []string
}

func (s *callingStore) AppendNotes(id, text string) error {
	s.calls = append(s.calls, "AppendNotes "+id+" "+text)
	return nil
}

// outboxNotes returns the bytes of the seat's outbox notes file, or nil when it
// does not exist.
func outboxNotes(t *testing.T, root string) []byte {
	t.Helper()
	notes, err := os.ReadFile(filepath.Join(root, outbox.Dir, outbox.NotesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return notes
}

func TestNoBeadAccessNoteAppendsToTheOutbox(t *testing.T) {
	t.Parallel()
	root := newFileModeSeat(t, "gt-own1")
	v, store, out := seatVerbs(t, root)
	for _, note := range []string{"the retry never resets its counter", "the gate is green"} {
		if err := v.note("gt-own1", note); err != nil {
			t.Fatal(err)
		}
	}
	want := "the retry never resets its counter\nthe gate is green\n"
	notes := outboxNotes(t, root)
	if string(notes) != want {
		t.Errorf("outbox notes = %q, want %q", notes, want)
	}
	// The host decodes what the seat wrote with outbox.Validate; the file the
	// verb left must already satisfy it.
	if _, err := outbox.Validate([]byte(`{"bead_id":"gt-own1","status":"done"}`), notes, "gt-own1"); err != nil {
		t.Errorf("the outbox the seat wrote does not validate: %v", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written in file mode: %v", store.calls)
	}
	if got, want := out.String(), "✓ Noted on gt-own1\n✓ Noted on gt-own1\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestNoBeadAccessNoteRefusesAnotherBead(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"gt-other", "gt-own", "gt-own1 "} {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			root := newFileModeSeat(t, "gt-own1")
			v, store, _ := seatVerbs(t, root)
			err := v.note(id, "a note")
			if err == nil {
				t.Fatalf("noting on %q succeeded in a seat that may write gt-own1", id)
			}
			if !strings.Contains(err.Error(), "only its own bead") || !strings.Contains(err.Error(), "gt-own1") {
				t.Errorf("error %q does not name the seat's own bead", err)
			}
			if notes := outboxNotes(t, root); notes != nil {
				t.Errorf("the outbox holds %q after a refused note", notes)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written for a refused note: %v", store.calls)
			}
		})
	}
}

// failingOutboxFS fails the step its field names and runs every other step on
// the real filesystem, so one row is one way a seat is killed or a disk fails
// partway through the append.
type failingOutboxFS struct {
	osOutboxFS
	readErr, mkdirErr, writeErr, renameErr, removeErr error
	// partialWrite writes half the data to the target and then fails, the way
	// a killed seat leaves a file it was in the middle of truncating.
	partialWrite bool
}

func (f failingOutboxFS) ReadFile(name string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.osOutboxFS.ReadFile(name)
}

func (f failingOutboxFS) MkdirAll(path string, perm fs.FileMode) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	return f.osOutboxFS.MkdirAll(path, perm)
}

func (f failingOutboxFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if f.writeErr != nil {
		if f.partialWrite {
			if err := f.osOutboxFS.WriteFile(name, data[:len(data)/2], perm); err != nil {
				return err
			}
		}
		return f.writeErr
	}
	return f.osOutboxFS.WriteFile(name, data, perm)
}

func (f failingOutboxFS) Rename(oldpath, newpath string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	return f.osOutboxFS.Rename(oldpath, newpath)
}

func (f failingOutboxFS) Remove(name string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	return f.osOutboxFS.Remove(name)
}

func TestNoBeadAccessNoteFailedWriteKeepsThePreviousNotes(t *testing.T) {
	t.Parallel()
	boom := errors.New("the seat was killed mid-write")
	const previous = "the first note\n"
	cases := map[string]failingOutboxFS{
		"read":          {readErr: boom},
		"mkdir":         {mkdirErr: boom},
		"write":         {writeErr: boom},
		"partial write": {writeErr: boom, partialWrite: true},
		"rename":        {renameErr: boom},
		// The write fails and the temporary file it left will not go either.
		"write and a stuck temp": {writeErr: boom, removeErr: boom},
	}
	for name, fsys := range cases {
		name, fsys := name, fsys
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := newFileModeSeat(t, "gt-own1")
			dir := filepath.Join(root, outbox.Dir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			notesPath := filepath.Join(dir, outbox.NotesFile)
			if err := os.WriteFile(notesPath, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			v, store, _ := seatVerbs(t, root)
			v.outboxFS = fsys
			if err := v.note("gt-own1", "the second note"); err == nil {
				t.Fatal("a note whose write failed reported success")
			}
			if got := outboxNotes(t, root); string(got) != previous {
				t.Errorf("outbox notes = %q after a failed write, want the previous %q", got, previous)
			}
			// The host's copy refuses a name in the outbox directory that is
			// not one of its two files, so a killed write must leave none there.
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != outbox.NotesFile {
				names := make([]string, len(entries))
				for i, e := range entries {
					names[i] = e.Name()
				}
				t.Errorf("the outbox directory holds %v after a failed write, want only %s", names, outbox.NotesFile)
			}
			// The failed call released the lock: the next note lands.
			v.outboxFS = nil
			if err := v.note("gt-own1", "the second note"); err != nil {
				t.Fatalf("the note after a failed one failed too, so the lock was not released: %v", err)
			}
			if got, want := string(outboxNotes(t, root)), previous+"the second note\n"; got != want {
				t.Errorf("outbox notes = %q after the retry, want %q", got, want)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written after a failed outbox write: %v", store.calls)
			}
		})
	}
}

func TestNoBeadAccessNoteRefusesAnUntakeableOutboxLock(t *testing.T) {
	t.Parallel()
	root := newFileModeSeat(t, "gt-own1")
	// A directory where the lock file belongs makes the flock unopenable, as a
	// stuck or damaged lock path would.
	if err := os.Mkdir(filepath.Join(root, ".gt", outboxLockFile), 0o755); err != nil {
		t.Fatal(err)
	}
	v, store, _ := seatVerbs(t, root)
	err := v.note("gt-own1", "a note")
	if err == nil {
		t.Fatal("a note was accepted while the outbox lock could not be taken")
	}
	if !strings.Contains(err.Error(), "outbox lock") {
		t.Errorf("error %q does not name the outbox lock", err)
	}
	if notes := outboxNotes(t, root); notes != nil {
		t.Errorf("the outbox holds %q after a refused note", notes)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written when the lock failed: %v", store.calls)
	}
}

func TestNoBeadAccessNoteRefusesATaskWithoutABead(t *testing.T) {
	t.Parallel()
	overwrite := func(content string) func(t *testing.T, root string) {
		return func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, seatTaskFile), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := map[string]func(t *testing.T, root string){
		"missing": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, seatTaskFile)); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, seatTaskFile)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, seatTaskFile), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"no heading":           overwrite("Prose with no task heading.\n"),
		"heading without id":   overwrite("# TASK: a task\n"),
		"heading with a space": overwrite("# TASK gt own1: a task\n"),
	}
	for name, arrange := range cases {
		name, arrange := name, arrange
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := newFileModeSeat(t, "gt-own1")
			arrange(t, root)
			v, store, _ := seatVerbs(t, root)
			err := v.note("gt-own1", "a note")
			if err == nil {
				t.Fatal("a note was accepted in a seat whose TASK.md names no bead")
			}
			if !strings.Contains(err.Error(), "TASK.md") {
				t.Errorf("error %q does not name TASK.md", err)
			}
			if notes := outboxNotes(t, root); notes != nil {
				t.Errorf("the outbox holds %q after a refused note", notes)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written when TASK.md named no bead: %v", store.calls)
			}
		})
	}
}

func TestNoBeadAccessNoteRefusesWhatTheOutboxWouldRefuse(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		previous string
		note     string
	}{
		"over the notes cap": {previous: strings.Repeat("a", 256<<10), note: "one more line"},
		"not UTF-8":          {previous: "the first note\n", note: "\xff the note"},
	}
	for name, c := range cases {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := newFileModeSeat(t, "gt-own1")
			dir := filepath.Join(root, outbox.Dir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, outbox.NotesFile), []byte(c.previous), 0o644); err != nil {
				t.Fatal(err)
			}
			v, store, _ := seatVerbs(t, root)
			err := v.note("gt-own1", c.note)
			if err == nil {
				t.Fatal("the note was accepted although the host would refuse the outbox")
			}
			if got := outboxNotes(t, root); string(got) != c.previous {
				t.Errorf("outbox notes changed to %q after a refused note", got)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written for a refused note: %v", store.calls)
			}
		})
	}
}

func TestNoBeadAccessNoteLeavesDoltModeAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir() // a seat root with no file-mode marker
	v, store, _ := seatVerbs(t, root)
	if err := v.note("gt-own1", "a note"); err != nil {
		t.Fatal(err)
	}
	const want = "AppendNotes gt-own1 a note"
	if len(store.calls) != 1 || store.calls[0] != want {
		t.Errorf("dolt-mode note calls = %v, want %q", store.calls, want)
	}
	if _, err := os.Stat(filepath.Join(root, outbox.Dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a dolt-mode note wrote an outbox: %v", err)
	}
}
