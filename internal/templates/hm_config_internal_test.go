package templates

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// faultyHMConfigTemp is a real temporary file that fails the step named: its
// write, its mode change or its close. The file is still created and closed for
// real, so a failure leaves the same files a real one would.
type faultyHMConfigTemp struct {
	*os.File
	write, chmod, closed error
}

func (f faultyHMConfigTemp) Write(p []byte) (int, error) {
	if f.write != nil {
		return 0, f.write
	}
	return f.File.Write(p)
}

func (f faultyHMConfigTemp) Chmod(mode os.FileMode) error {
	if f.chmod != nil {
		return f.chmod
	}
	return f.File.Chmod(mode)
}

func (f faultyHMConfigTemp) Close() error {
	err := f.File.Close()
	if f.closed != nil {
		return f.closed
	}
	return err
}

// TestWriteHMConfigFileKeepsTheOldConfigWhenAStepFails: a temporary file that
// cannot be made, a write, a mode change, a close or the rename that fails
// leaves the old config as it was, leaves no temporary file behind, and is what
// the caller gets.
func TestWriteHMConfigFileKeepsTheOldConfigWhenAStepFails(t *testing.T) {
	t.Parallel()
	failure := errors.New("injected failure")
	const old = "old config\n"
	tests := []struct {
		name           string
		write, chmod   error
		closed         error
		create, rename bool
	}{
		{name: "the temporary file cannot be made", create: true},
		{name: "the write fails", write: failure},
		{name: "the mode change fails", chmod: failure},
		{name: "the close fails", closed: failure},
		{name: "the rename fails", rename: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "hm-config.json")
			if tt.rename {
				// Renaming a file onto a directory fails whatever its contents.
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("making the directory the rename cannot replace: %v", err)
				}
			} else if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
				t.Fatalf("writing the config the derived one replaces: %v", err)
			}
			create := func(dir, pattern string) (hmConfigTemp, error) {
				if tt.create {
					return nil, failure
				}
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return faultyHMConfigTemp{File: f, write: tt.write, chmod: tt.chmod, closed: tt.closed}, nil
			}
			err := writeHMConfigFile(path, []byte("derived\n"), create)
			if tt.rename {
				if err == nil {
					t.Fatal("writeHMConfigFile replaced a directory with a file")
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("writeHMConfigFile error = %v, want the injected failure", err)
			}
			if !tt.rename {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading the config: %v", err)
				}
				if !reflect.DeepEqual(got, []byte(old)) {
					t.Errorf("the config = %q, want the old one unchanged", got)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("reading %s: %v", dir, err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if want := []string{"hm-config.json"}; !reflect.DeepEqual(names, want) {
				t.Errorf("%s holds %v after a failed write, want %v", dir, names, want)
			}
		})
	}
}
