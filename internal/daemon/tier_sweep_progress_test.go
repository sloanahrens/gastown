package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Data that is not a JSON object is refused, and the record already on disk
// stays as it was.
func TestWriteRunProgressRefusesNonObjectAndKeepsOldRecord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data string
	}{
		{"empty", ``},
		{"array", `[1,2]`},
		{"number", `42`},
		{"string", `"done"`},
		{"null", `null`},
		{"truncated object", `{"tiers_done":`},
		{"object then trailing text", `{} x`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			old := []byte(`{"keep":true}`)
			if err := writeRunProgress(dir, old); err != nil {
				t.Fatal(err)
			}
			if err := writeRunProgress(dir, []byte(tc.data)); err == nil {
				t.Fatalf("writeRunProgress(%q) = nil, want an error", tc.data)
			}
			got, err := os.ReadFile(filepath.Join(dir, runProgressName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, old) {
				t.Fatalf("progress.json = %q after a refused write, want the old %q", got, old)
			}
		})
	}
}

// When the rename onto progress.json fails, the error names the directory and
// the temp file is removed, so nothing is left beside the record.
func TestWriteRunProgressRenameFailureRemovesTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory where the record goes makes the rename of a file onto it fail.
	if err := os.Mkdir(filepath.Join(dir, runProgressName), 0o700); err != nil {
		t.Fatal(err)
	}
	err := writeRunProgress(dir, []byte(`{"tiers_done":1}`))
	if err == nil {
		t.Fatal("writeRunProgress over a directory = nil, want an error")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q does not name the directory %q", err, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != runProgressName {
		t.Fatalf("dir after a failed rename holds %d entries, want only the directory %s", len(entries), runProgressName)
	}
}

// errProgressInjected is the failure a test puts in one step.
var errProgressInjected = errors.New("injected failure")

// failingProgressTemp fails one call on a real temp file: Write, Sync or Close.
// Only its Close wrapper closes the real file; on a Write or Sync failure the
// writer's own Close call does, so the test leaks no descriptor.
type failingProgressTemp struct {
	progressTemp
	step string
}

func (f failingProgressTemp) Write(p []byte) (int, error) {
	if f.step == "write" {
		return 0, errProgressInjected
	}
	return f.progressTemp.Write(p)
}

func (f failingProgressTemp) Sync() error {
	if f.step == "sync" {
		return errProgressInjected
	}
	return f.progressTemp.Sync()
}

func (f failingProgressTemp) Close() error {
	err := f.progressTemp.Close()
	if f.step == "close" {
		return errProgressInjected
	}
	return err
}

// A failure in any step before the rename returns an error naming the
// directory, keeps the old record whole, and leaves no temp file beside it.
func TestWriteRunProgressFailedStepKeepsOldRecordAndRemovesTemp(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"createtemp", "write", "sync", "close", "chmod"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			old := []byte(`{"keep":true}`)
			if err := writeRunProgress(dir, old); err != nil {
				t.Fatal(err)
			}
			fs := progressFS{
				createTemp: func(d, pattern string) (progressTemp, error) {
					if step == "createtemp" {
						return nil, errProgressInjected
					}
					tmp, err := os.CreateTemp(d, pattern)
					if err != nil {
						return nil, err
					}
					return failingProgressTemp{progressTemp: tmp, step: step}, nil
				},
				chmod: func(name string, mode os.FileMode) error {
					if step == "chmod" {
						return errProgressInjected
					}
					return os.Chmod(name, mode)
				},
			}
			err := writeRunProgressFS(fs, dir, []byte(`{"tiers_done":2}`))
			if !errors.Is(err, errProgressInjected) {
				t.Fatalf("error = %v, want the injected failure", err)
			}
			if !strings.Contains(err.Error(), dir) {
				t.Fatalf("error %q does not name the directory %q", err, dir)
			}
			got, err := os.ReadFile(filepath.Join(dir, runProgressName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, old) {
				t.Fatalf("progress.json = %q after a failed %s, want the old %q", got, step, old)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("dir after a failed %s holds %d entries, want 1", step, len(entries))
			}
		})
	}
}

// A directory that does not exist is an error that names it, and the write
// does not create it.
func TestWriteRunProgressRefusesMissingDirectory(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	err := writeRunProgress(missing, []byte(`{}`))
	if err == nil {
		t.Fatal("writeRunProgress into a missing directory = nil, want an error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name the directory %q", err, missing)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("the missing directory was created: stat err = %v", statErr)
	}
}
