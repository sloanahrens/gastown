package ominstaller

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFiles is an in-memory Files. Each failure is set by a test; everything
// else succeeds. writes and renames record what was asked, in order, so a test
// can see that a copy went to a temp file and was renamed into place.
type fakeFiles struct {
	files map[string][]byte
	modes map[string]fs.FileMode

	statErr   map[string]error
	readErr   map[string]error
	removeErr map[string]error

	writeErr  error
	chmodErr  error
	renameErr error
	// partial is what a failed WriteFile leaves in its file: a copy that died
	// mid-write.
	partial []byte

	writes  []string
	renames []string
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{
		files:     map[string][]byte{},
		modes:     map[string]fs.FileMode{},
		statErr:   map[string]error{},
		readErr:   map[string]error{},
		removeErr: map[string]error{},
	}
}

func notExist(op, path string) error {
	return &fs.PathError{Op: op, Path: path, Err: fs.ErrNotExist}
}

func (f *fakeFiles) Stat(path string) (fs.FileMode, error) {
	if err := f.statErr[path]; err != nil {
		return 0, err
	}
	if _, ok := f.files[path]; !ok {
		return 0, notExist("stat", path)
	}
	if mode, ok := f.modes[path]; ok {
		return mode, nil
	}
	return 0o644, nil
}

// Lstat is Stat: the fake holds no symbolic links. The rows that need a link use
// the real filesystem, in a temporary directory.
func (f *fakeFiles) Lstat(path string) (fs.FileMode, error) {
	return f.Stat(path)
}

func (f *fakeFiles) ReadFile(path string) ([]byte, error) {
	if err := f.readErr[path]; err != nil {
		return nil, err
	}
	data, ok := f.files[path]
	if !ok {
		return nil, notExist("open", path)
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeFiles) WriteFile(path string, data []byte, perm fs.FileMode) error {
	f.writes = append(f.writes, path)
	if f.writeErr != nil {
		if f.partial != nil {
			f.files[path] = f.partial
		}
		return f.writeErr
	}
	f.files[path] = append([]byte(nil), data...)
	f.modes[path] = perm
	return nil
}

func (f *fakeFiles) Chmod(path string, perm fs.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	if _, ok := f.files[path]; !ok {
		return notExist("chmod", path)
	}
	f.modes[path] = perm
	return nil
}

func (f *fakeFiles) Rename(oldpath, newpath string) error {
	f.renames = append(f.renames, oldpath+" -> "+newpath)
	if f.renameErr != nil {
		return f.renameErr
	}
	data, ok := f.files[oldpath]
	if !ok {
		return notExist("rename", oldpath)
	}
	f.files[newpath] = data
	f.modes[newpath] = f.modes[oldpath]
	delete(f.files, oldpath)
	delete(f.modes, oldpath)
	return nil
}

func (f *fakeFiles) Remove(path string) error {
	if err := f.removeErr[path]; err != nil {
		return err
	}
	if _, ok := f.files[path]; !ok {
		return notExist("remove", path)
	}
	delete(f.files, path)
	delete(f.modes, path)
	return nil
}

// logRecorder is a Logf seam that keeps each notice as one line.
type logRecorder struct {
	lines []string
}

func (l *logRecorder) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// warnings counts the WARNING lines, the notices an install that goes ad-hoc
// logs.
func (l *logRecorder) warnings() int {
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, "WARNING") {
			n++
		}
	}
	return n
}

// TestOSFilesStatReportsAnAbsentPathAsNotExist drives the real filesystem's
// stat over a path the test owns. The install's absent-om check reads that
// error, so it must still match fs.ErrNotExist.
func TestOSFilesStatReportsAnAbsentPathAsNotExist(t *testing.T) {
	t.Parallel()
	_, err := osFiles{}.Stat(filepath.Join(t.TempDir(), "no-such-om"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat of an absent path = %v, want fs.ErrNotExist", err)
	}
}

// errBoom is the failure a test injects where the reason does not matter.
var errBoom = errors.New("boom")
