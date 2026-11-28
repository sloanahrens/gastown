package townflight

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func TestEntry(t *testing.T) {
	t.Parallel()
	t.Run("holds the label", func(t *testing.T) {
		t.Parallel()
		e, err := createEntry(filepath.Join(t.TempDir(), "inflight"), "gt status")
		if err != nil {
			t.Fatalf("createEntry: %v", err)
		}
		defer e.drop() //nolint:errcheck // best-effort cleanup
		got, err := os.ReadFile(e.path)
		if err != nil {
			t.Fatalf("reading entry: %v", err)
		}
		if string(got) != "gt status" {
			t.Errorf("entry holds %q, want %q", got, "gt status")
		}
	})
	t.Run("stays locked until dropped", func(t *testing.T) {
		t.Parallel()
		e, err := createEntry(t.TempDir(), "gt status")
		if err != nil {
			t.Fatalf("createEntry: %v", err)
		}
		second := flock.New(e.path)
		if ok, err := second.TryLock(); err != nil || ok {
			t.Fatalf("second TryLock = %v, %v while entry held; want false", ok, err)
		}
		if err := e.drop(); err != nil {
			t.Fatalf("drop: %v", err)
		}
		if _, err := os.Stat(e.path); !os.IsNotExist(err) {
			t.Errorf("Stat after drop = %v, want not exist", err)
		}
		if err := e.drop(); err == nil {
			t.Error("a second drop returned no error")
		}
		if ok, err := second.TryLock(); err != nil || !ok {
			t.Errorf("second TryLock after drop = %v, %v; want true", ok, err)
		}
		_ = second.Unlock()
	})
	t.Run("unmakeable dir is an error", func(t *testing.T) {
		t.Parallel()
		blocked := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocked, nil, 0o600); err != nil {
			t.Fatalf("writing blocker: %v", err)
		}
		if _, err := createEntry(filepath.Join(blocked, "inflight"), "gt status"); err == nil {
			t.Error("createEntry below a regular file returned no error")
		}
	})
}

// TestEntryStepFailures takes each failure of a step openEntry performs and
// checks that its error reaches the caller with nothing left on disk.
func TestEntryStepFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := []struct {
		name  string
		files func() entryFiles
	}{
		{"create temp", func() entryFiles {
			files := osFiles()
			files.createTemp = func(string, string) (entryFile, error) { return nil, boom }
			return files
		}},
		{"lock", func() entryFiles {
			files := osFiles()
			files.newLock = func(string) entryLock { return failingLock{err: boom} }
			return files
		}},
		{"write", func() entryFiles {
			files := osFiles()
			files.createTemp = wrapTemp(func(f *os.File) entryFile {
				return failingFile{entryFile: f, writeErr: boom}
			})
			return files
		}},
		{"close", func() entryFiles {
			files := osFiles()
			files.createTemp = wrapTemp(func(f *os.File) entryFile {
				return failingFile{entryFile: f, closeErr: boom}
			})
			return files
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if _, err := openEntry(tc.files(), dir, "gt status"); !errors.Is(err, boom) {
				t.Fatalf("openEntry error = %v, want it to wrap %v", err, boom)
			}
			left, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("reading dir: %v", err)
			}
			if len(left) != 0 {
				t.Errorf("failed step left %d entries behind", len(left))
			}
		})
	}
}

// wrapTemp makes a createTemp that wraps the real file in wrap.
func wrapTemp(wrap func(*os.File) entryFile) func(string, string) (entryFile, error) {
	return func(dir, pattern string) (entryFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return wrap(f), nil
	}
}

// failingFile is a real open file whose write or close fails as set.
type failingFile struct {
	entryFile
	writeErr, closeErr error
}

func (f failingFile) WriteString(s string) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.entryFile.WriteString(s)
}

func (f failingFile) Close() error {
	if err := f.entryFile.Close(); err != nil {
		return err
	}
	return f.closeErr
}

// failingLock is a lock that fails to lock as set.
type failingLock struct{ err error }

func (f failingLock) Lock() error   { return f.err }
func (f failingLock) Unlock() error { return nil }
