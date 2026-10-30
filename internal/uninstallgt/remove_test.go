package uninstallgt

import (
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// entry is one path as a case sets it up: whether it is a directory, and how
// an operation on it behaves.
type entry struct {
	dir       bool  // Stat and Lstat report a directory
	link      bool  // Lstat reports a symlink, which Stat would follow
	lstatErr  error // Lstat fails with this instead
	statErr   error // Stat fails with this instead
	removeErr error // Remove fails with this
	survives  bool  // Remove answers nil and leaves the path present
}

// recorder is a case's filesystem and command runner: the paths the case sets
// up, and one log of every operation, so a test reads what a run did and in
// what order.
type recorder struct {
	entries map[string]entry
	// afterRemove is the error a path's Lstat reports once the path has been
	// removed, so a case can drive the confirmation read.
	afterRemove map[string]error
	ops         []string
}

func newRecorder() *recorder {
	return &recorder{entries: map[string]entry{}, afterRemove: map[string]error{}}
}

// add sets path's state.
func (r *recorder) add(path string, e entry) { r.entries[path] = e }

// removed lists the paths a run asked to remove, in order.
func (r *recorder) removed() []string {
	var paths []string
	for _, op := range r.ops {
		if path, ok := strings.CutPrefix(op, "remove "); ok {
			paths = append(paths, path)
		}
	}
	return paths
}

// has reports whether path is still there.
func (r *recorder) has(path string) bool {
	_, ok := r.entries[path]
	return ok
}

// Lstat reports path itself: its state as the case set it, the afterRemove
// error once it is gone, or fs.ErrNotExist.
func (r *recorder) Lstat(path string) (fs.FileInfo, error) {
	r.ops = append(r.ops, "lstat "+path)
	if e, ok := r.entries[path]; ok {
		if e.lstatErr != nil {
			return nil, e.lstatErr
		}
		return info{name: filepath.Base(path), dir: e.dir, link: e.link}, nil
	}
	if err := r.afterRemove[path]; err != nil {
		return nil, err
	}
	return nil, fs.ErrNotExist
}

// Stat reports path, following a symlink; the run asks it about the install
// directory only.
func (r *recorder) Stat(path string) (fs.FileInfo, error) {
	r.ops = append(r.ops, "stat "+path)
	e, ok := r.entries[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if e.statErr != nil {
		return nil, e.statErr
	}
	return info{name: filepath.Base(path), dir: e.dir}, nil
}

// Remove records the call and removes the path, unless the case says it
// survives.
func (r *recorder) Remove(path string) error {
	r.ops = append(r.ops, "remove "+path)
	e, ok := r.entries[path]
	if !ok {
		return fs.ErrNotExist
	}
	if e.removeErr != nil {
		return e.removeErr
	}
	if !e.survives {
		delete(r.entries, path)
	}
	return nil
}

// exec records the command a run would start and answers that it ran.
func (r *recorder) exec(name string, args ...string) error {
	r.ops = append(r.ops, "exec "+strings.Join(append([]string{name}, args...), " "))
	return nil
}

// info is what a run asks of a FileInfo: whether the path is a directory.
type info struct {
	name string
	dir  bool
	link bool
}

func (i info) Name() string { return i.name }
func (i info) Size() int64  { return 0 }

func (i info) Mode() fs.FileMode {
	switch {
	case i.link:
		return fs.ModeSymlink | 0o777
	case i.dir:
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.dir }
func (i info) Sys() any           { return nil }

// runHost is one case's install directory and home, both under its own
// temporary tree.
type runHost struct {
	home string
	bin  string
}

func newRunHost(t *testing.T) runHost {
	t.Helper()
	base := t.TempDir()
	return runHost{home: filepath.Join(base, "home"), bin: filepath.Join(base, "bin")}
}

// env is the run's outside world: the recorder behind both seams, this case's
// home, and no town.
func (h runHost) env(rec *recorder, extra map[string]string) Env {
	m := map[string]string{"HOME": h.home}
	for key, value := range extra {
		m[key] = value
	}
	return Env{Getenv: vars(m), Files: rec, Exec: rec.exec, Signal0: neverSignals}
}

// TestRemoveTouchesExactlyGtAndBd pins the contract that makes the recipe safe
// after the incident (gt-acdfp): a run removes <dir>/gt and <dir>/bd and
// nothing else, so a file beside them and the directory itself survive.
func TestRemoveTouchesExactlyGtAndBd(t *testing.T) {
	t.Parallel()
	h := newRunHost(t)
	gt, bd := filepath.Join(h.bin, "gt"), filepath.Join(h.bin, "bd")
	unrelated := filepath.Join(h.bin, "unrelated")
	rec := newRecorder()
	rec.add(h.bin, entry{dir: true})
	rec.add(gt, entry{})
	rec.add(bd, entry{})
	rec.add(unrelated, entry{})

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.bin}, &stdout, &stderr, h.env(rec, nil))

	if code != ExitOK {
		t.Fatalf("Run on a directory holding gt and bd = %d, want %d (stderr: %s)",
			code, ExitOK, stderr.String())
	}
	if got, want := rec.removed(), []string{gt, bd}; !reflect.DeepEqual(got, want) {
		t.Errorf("Remove was called on %v, want exactly %v", got, want)
	}
	if !rec.has(unrelated) {
		t.Error("an unrelated file beside the two binaries was removed")
	}
}

// TestImmutableFlagClearedBeforeRemoval pins the order the installer's
// OS-immutable flag needs (gt-vya0s): the flag comes off each path before the
// removal is attempted, or the kernel refuses the unlink.
func TestImmutableFlagClearedBeforeRemoval(t *testing.T) {
	t.Parallel()
	h := newRunHost(t)
	rec := newRecorder()
	rec.add(h.bin, entry{dir: true})
	rec.add(filepath.Join(h.bin, "gt"), entry{})
	rec.add(filepath.Join(h.bin, "bd"), entry{})

	var stdout, stderr strings.Builder
	if code := Run([]string{"--install-dir", h.bin}, &stdout, &stderr, h.env(rec, nil)); code != ExitOK {
		t.Fatalf("Run = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}

	// The two names are spelled here rather than read from names, so a run
	// that stopped removing one of them fails this test.
	for _, name := range []string{"gt", "bd"} {
		path := filepath.Join(h.bin, name)
		removeAt, chflagsAt := -1, -1
		for i, op := range rec.ops {
			switch {
			case op == "remove "+path:
				removeAt = i
			case chflagsAt < 0 && strings.HasPrefix(op, "exec chflags ") && strings.HasSuffix(op, " "+path):
				chflagsAt = i
			}
		}
		if removeAt < 0 {
			t.Fatalf("%s was never removed (ops: %v)", path, rec.ops)
		}
		if chflagsAt < 0 {
			t.Fatalf("%s: no chflags before the removal (ops: %v)", path, rec.ops)
		}
		if chflagsAt > removeAt {
			t.Errorf("%s: the flag was cleared at %d, after the removal at %d", path, chflagsAt, removeAt)
		}
	}
}

// touches lists, in order, the operations in ops that clear path's flag or
// remove path, so a test reads what was done to one path and nothing else.
func touches(ops []string, path string) []string {
	var got []string
	for _, op := range ops {
		if op == "remove "+path || strings.HasPrefix(op, "exec ") && strings.HasSuffix(op, " "+path) {
			got = append(got, op)
		}
	}
	return got
}

// TestImmutableFallbacksRunForARegularFile pins the full flag sequence for a
// regular file: the -h attempt, then both fallbacks, then the removal.
func TestImmutableFallbacksRunForARegularFile(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	path := "/install/gt"
	rec.add(path, entry{})

	if err := Remove(rec, rec.exec, path); err != nil {
		t.Fatalf("Remove = %v, want nil", err)
	}

	want := []string{
		"exec chflags -h nouchg " + path,
		"exec chflags nouchg " + path,
		"exec chattr -i " + path,
		"remove " + path,
	}
	if got := touches(rec.ops, path); !reflect.DeepEqual(got, want) {
		t.Errorf("operations on a regular file = %v, want %v", got, want)
	}
}

// TestImmutableFallbacksSkipASymlink pins the symlink case: the fallbacks
// follow a link to its target, so a symlinked bd gets the -h attempt alone and
// the file it points at is never named by a flag command.
func TestImmutableFallbacksSkipASymlink(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	path := "/install/bd"
	rec.add(path, entry{link: true})

	if err := Remove(rec, rec.exec, path); err != nil {
		t.Fatalf("Remove = %v, want nil", err)
	}

	want := []string{"exec chflags -h nouchg " + path, "remove " + path}
	if got := touches(rec.ops, path); !reflect.DeepEqual(got, want) {
		t.Errorf("operations on a symlink = %v, want %v", got, want)
	}
}

func TestRemoveReportsAnAbsentPath(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	path := "/install/gt"

	err := Remove(rec, rec.exec, path)

	if !errors.Is(err, ErrAbsent) {
		t.Errorf("Remove of an absent path = %v, want ErrAbsent", err)
	}
	if got := rec.removed(); len(got) != 0 {
		t.Errorf("Remove of an absent path called Remove on %v, want nothing", got)
	}
}

func TestRemoveReportsAReadError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	path := "/install/gt"
	rec.add(path, entry{lstatErr: fs.ErrPermission})

	err := Remove(rec, rec.exec, path)

	if err == nil || errors.Is(err, ErrAbsent) {
		t.Errorf("Remove of an unreadable path = %v, want an error other than ErrAbsent", err)
	}
	if got := rec.removed(); len(got) != 0 {
		t.Errorf("Remove of an unreadable path called Remove on %v, want nothing", got)
	}
}

func TestRemoveReportsARemoveError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	path := "/install/gt"
	rec.add(path, entry{removeErr: fs.ErrPermission})

	err := Remove(rec, rec.exec, path)

	if err == nil || errors.Is(err, ErrAbsent) {
		t.Errorf("Remove of a path the filesystem refused to remove = %v, want an error", err)
	}
}

func TestRemoveFailsWhenThePathSurvives(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	path := "/install/gt"
	rec.add(path, entry{survives: true})

	err := Remove(rec, rec.exec, path)

	if err == nil || errors.Is(err, ErrAbsent) {
		t.Errorf("Remove of a path still there after the removal = %v, want a failure", err)
	}
}

func TestRemoveReportsTheConfirmationError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	path := "/install/gt"
	rec.add(path, entry{})
	rec.afterRemove[path] = fs.ErrPermission

	err := Remove(rec, rec.exec, path)

	if err == nil || errors.Is(err, ErrAbsent) {
		t.Errorf("Remove whose confirmation read failed = %v, want the read's error", err)
	}
}
