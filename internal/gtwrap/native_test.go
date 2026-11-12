package gtwrap

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statFiles answers stat from a map of path to file mode; a missing path is
// fs.ErrNotExist.
func statFiles(files map[string]os.FileMode) func(string) (fs.FileInfo, error) {
	return func(path string) (fs.FileInfo, error) {
		mode, ok := files[path]
		if !ok {
			return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
		}
		return fakeInfo{name: filepath.Base(path), mode: mode}, nil
	}
}

func TestNativeCoversOnlyItsSubcommands(t *testing.T) {
	t.Parallel()
	n := Native{Tool: "om", Path: "/usr/local/bin/om", Subcommands: []string{"review"}}
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "review", args: []string{"review", "-base", "origin/main"}, want: true},
		{name: "status", args: []string{"status"}, want: false},
		{name: "no arguments", args: nil, want: false},
		{name: "review named after a flag", args: []string{"--json", "review"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := n.Covers(tt.args); got != tt.want {
				t.Fatalf("Covers(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestNativeCheckRefusesAPathThatCannotRun(t *testing.T) {
	t.Parallel()
	exe := filepath.Join(t.TempDir(), "om")
	plain := filepath.Join(t.TempDir(), "om")
	tests := []struct {
		name    string
		path    string
		files   map[string]os.FileMode
		wantErr bool
	}{
		{name: "runnable file", path: exe, files: map[string]os.FileMode{exe: 0o755}},
		{name: "empty path", path: "", wantErr: true},
		{name: "relative path", path: "bin/om", files: map[string]os.FileMode{"bin/om": 0o755}, wantErr: true},
		{name: "missing file", path: exe, wantErr: true},
		{name: "not executable", path: plain, files: map[string]os.FileMode{plain: 0o644}, wantErr: true},
		{name: "a directory", path: exe, files: map[string]os.FileMode{exe: fs.ModeDir | 0o755}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := Native{Tool: "om", Path: tt.path, Subcommands: []string{"review"}}
			err := n.Check(statFiles(tt.files))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Check() = %v, want an error: %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "native.om.path") {
				t.Fatalf("Check() error %q does not name the config key", err)
			}
		})
	}
}

func TestNativeCheckReportsAStatFailure(t *testing.T) {
	t.Parallel()
	n := Native{Tool: "om", Path: "/opt/om", Subcommands: []string{"review"}}
	boom := errors.New("permission denied")
	err := n.Check(func(string) (fs.FileInfo, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Check() = %v, want the stat error to be carried", err)
	}
}
