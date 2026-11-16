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

// newNoteSeat returns the root of a seat in file mode whose TASK.md names
// beadID, with the runtime archive's marker and task file written under it.
func newNoteSeat(t *testing.T, beadID string) string {
	t.Helper()
	root := t.TempDir()
	taskDir := filepath.Join(root, seatTaskPath)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, taskfile.ModeFile), []byte(taskfile.ModeFiles+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, seatTaskFilePath), []byte("# TASK "+beadID+": a task\n"), 0o644); err != nil {
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
	return seatVerbsAt(t, filepath.Join(root, "gt", "gastown", "polecats", "slit", "gastown"))
}

// seatVerbsAt is seatVerbs with the working directory named, for the callers
// that must run from outside a seat's town root.
func seatVerbsAt(t *testing.T, worktree string) (*beadVerbs, *callingStore, *bytes.Buffer) {
	t.Helper()
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
// or Update it records is a bead write that escaped the outbox.
type callingStore struct {
	beads.Client
	calls []string
}

func (s *callingStore) Update(id string, opts beads.UpdateOptions) error {
	s.calls = append(s.calls, "Update "+id)
	return nil
}

func (s *callingStore) AppendNotes(id, text string) error {
	s.calls = append(s.calls, "AppendNotes "+id+" "+text)
	return nil
}

func (s *callingStore) AddComment(id, text string) error {
	s.calls = append(s.calls, "AddComment "+id+" "+text)
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
	root := newNoteSeat(t, "gt-own1")
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
			root := newNoteSeat(t, "gt-own1")
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

// A comment in file mode reaches the seat's outbox on every call and never the
// bead store, which file mode forbids (gt-jqo63z).
func TestNoBeadAccessCommentAppendsToTheOutbox(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	v, store, out := seatVerbs(t, root)
	for _, comment := range []string{"the retry never resets its counter", "the gate is green"} {
		if err := v.comment("gt-own1", comment); err != nil {
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
	if got, want := out.String(), "✓ Commented on gt-own1\n✓ Commented on gt-own1\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestNoBeadAccessCommentRefusesAnotherBead(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"gt-other", "gt-own", "gt-own1 "} {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			root := newNoteSeat(t, "gt-own1")
			v, store, _ := seatVerbs(t, root)
			err := v.comment(id, "a comment")
			if err == nil {
				t.Fatalf("commenting on %q succeeded in a seat that may write gt-own1", id)
			}
			if !strings.Contains(err.Error(), "only its own bead") || !strings.Contains(err.Error(), "gt-own1") {
				t.Errorf("error %q does not name the seat's own bead", err)
			}
			if !strings.Contains(err.Error(), "commenting") {
				t.Errorf("error %q reads as another verb", err)
			}
			if notes := outboxNotes(t, root); notes != nil {
				t.Errorf("the outbox holds %q after a refused comment", notes)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written for a refused comment: %v", store.calls)
			}
		})
	}
}

// A seat whose own bead cannot be read cannot tell whether the comment names
// that bead, so the comment refuses rather than falling through to a bead write
// (gt-jqo63z).
func TestNoBeadAccessCommentRefusesAnUnreadableSeat(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	if err := os.Remove(filepath.Join(root, seatTaskFilePath)); err != nil {
		t.Fatal(err)
	}
	v, store, _ := seatVerbs(t, root)
	err := v.comment("gt-own1", "a comment")
	if err == nil {
		t.Fatal("a comment was accepted while TASK.md could not be read")
	}
	if !strings.Contains(err.Error(), "TASK.md") {
		t.Errorf("error %q does not say TASK.md cannot be read", err)
	}
	if notes := outboxNotes(t, root); notes != nil {
		t.Errorf("the outbox holds %q after a refused comment", notes)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written when TASK.md could not be read: %v", store.calls)
	}
}

func TestNoBeadAccessCommentLeavesDoltModeAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir() // a seat root with no file-mode marker
	v, store, _ := seatVerbs(t, root)
	if err := v.comment("gt-own1", "a comment"); err != nil {
		t.Fatal(err)
	}
	const want = "AddComment gt-own1 a comment"
	if len(store.calls) != 1 || store.calls[0] != want {
		t.Errorf("dolt-mode comment calls = %v, want %q", store.calls, want)
	}
	if _, err := os.Stat(filepath.Join(root, outbox.Dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a dolt-mode comment wrote an outbox: %v", err)
	}
}

// A comment whose outbox write fails reports the failure as a comment and
// leaves the notes the seat already had, so a killed append never loses them
// (gt-jqo63z).
func TestNoBeadAccessCommentRefusesAFailedOutboxWrite(t *testing.T) {
	t.Parallel()
	boom := errors.New("the seat was killed mid-write")
	const previous = "the first comment\n"
	for name, fsys := range map[string]failingOutboxFS{
		"write":  {writeErr: boom},
		"rename": {renameErr: boom},
	} {
		name, fsys := name, fsys
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := newNoteSeat(t, "gt-own1")
			dir := filepath.Join(root, outbox.Dir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, outbox.NotesFile), []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			v, store, _ := seatVerbs(t, root)
			v.outboxFS = fsys
			err := v.comment("gt-own1", "the second comment")
			if err == nil {
				t.Fatal("a comment whose write failed reported success")
			}
			if !strings.Contains(err.Error(), "commenting") {
				t.Errorf("error %q does not read as a failed comment", err)
			}
			if got := outboxNotes(t, root); string(got) != previous {
				t.Errorf("outbox notes = %q after a failed write, want the previous %q", got, previous)
			}
			// The host's copy refuses a name in the outbox directory that is
			// not one of its two files, so a killed write must leave none there.
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || entries[0].Name() != outbox.NotesFile {
				names := make([]string, len(entries))
				for i, e := range entries {
					names[i] = e.Name()
				}
				t.Errorf("the outbox directory holds %v after a failed write, want only %s", names, outbox.NotesFile)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written after a failed outbox write: %v", store.calls)
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
			root := newNoteSeat(t, "gt-own1")
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
	root := newNoteSeat(t, "gt-own1")
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
			if err := os.WriteFile(filepath.Join(root, seatTaskFilePath), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := map[string]func(t *testing.T, root string){
		"missing": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, seatTaskFilePath)); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, seatTaskFilePath)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, seatTaskFilePath), 0o755); err != nil {
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
			root := newNoteSeat(t, "gt-own1")
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
			root := newNoteSeat(t, "gt-own1")
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

// A files-mode marker that exists but cannot be read leaves the mode unknown.
// The note refuses there rather than taking dolt mode and writing the bead,
// which file mode forbids (gt-w9male).
func TestNoBeadAccessNoteRefusesAnUnreadableMarker(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	marker := filepath.Join(root, seatTaskPath, taskfile.ModeFile)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// A directory where the marker belongs cannot be read as its content.
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	v, store, _ := seatVerbs(t, root)
	err := v.note("gt-own1", "a note")
	if err == nil {
		t.Fatal("a note was accepted while the file-mode marker could not be read")
	}
	if !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("error %q does not say the marker cannot be read", err)
	}
	if notes := outboxNotes(t, root); notes != nil {
		t.Errorf("the outbox holds %q after a refused note", notes)
	}
	if len(store.calls) != 0 {
		t.Errorf("the bead store was written when the marker could not be read: %v", store.calls)
	}
}

// A files-mode marker outside the caller's own town root is another run's
// left-over. It must not turn a caller elsewhere on the host into a seat and
// redirect its notes to that outbox (gt-w9male).
func TestNoBeadAccessNoteRefusesAMarkerOutsideItsTownRoot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, worktree string }{
		{"a sibling directory", "work"},
		{"the seat root itself", "."},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newNoteSeat(t, "gt-own1")
			v, store, _ := seatVerbsAt(t, filepath.Join(root, tc.worktree))
			err := v.note("gt-own1", "a note")
			if err == nil {
				t.Fatalf("a marker at %s/ made the caller at %s its seat", root, tc.worktree)
			}
			if !strings.Contains(err.Error(), "sandbox town root") {
				t.Errorf("error %q does not name the sandbox town root", err)
			}
			if notes := outboxNotes(t, root); notes != nil {
				t.Errorf("the stray marker's outbox holds %q", notes)
			}
			if len(store.calls) != 0 {
				t.Errorf("the bead store was written for a refused note: %v", store.calls)
			}
		})
	}
}

// The archive stages its marker only above the town root. A marker naming
// another mode inside the seat's town tree is not the archive's, and it must
// not turn a file-mode seat's writes back into bead writes (gt-w9male).
func TestNoBeadAccessNoteIgnoresAMarkerNamingAnotherMode(t *testing.T) {
	t.Parallel()
	root := newNoteSeat(t, "gt-own1")
	// A marker inside the town root, between the seat root and the worktree.
	inner := filepath.Join(root, seatTownDir, "gastown", seatTaskPath)
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, taskfile.ModeFile), []byte("dolt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, store, _ := seatVerbs(t, root)
	if err := v.note("gt-own1", "a note"); err != nil {
		t.Fatal(err)
	}
	if got, want := string(outboxNotes(t, root)), "a note\n"; got != want {
		t.Errorf("outbox notes = %q, want %q", got, want)
	}
	if len(store.calls) != 0 {
		t.Errorf("an inner marker turned the seat's note into a bead write: %v", store.calls)
	}
}
