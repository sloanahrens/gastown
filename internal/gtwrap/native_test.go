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

func TestNativeCoversGtHostCommands(t *testing.T) {
	t.Parallel()
	n := Native{Tool: "gt", Path: "/usr/local/bin/gt", Subcommands: gtHostCommands}
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "town forward plist", args: []string{"town", "forward", "plist"}, want: true},
		{name: "town forward plist with flags", args: []string{"town", "forward", "plist", "--service", "dashboard", "--port", "8080"}, want: true},
		{name: "the town forward group alone", args: []string{"town", "forward"}, want: false},
		{name: "town state", args: []string{"town", "state"}, want: false},
		{name: "pool show", args: []string{"pool", "show"}, want: false},
		{name: "status", args: []string{"status"}, want: false},
		{name: "maint plan", args: []string{"maint", "plan"}, want: true},
		{name: "maint apply", args: []string{"maint", "apply", "baks"}, want: true},
		{name: "maint plan nodes", args: []string{"maint", "plan", "nodes"}, want: true},
		{name: "maint plan colima", args: []string{"maint", "plan", "colima"}, want: true},
		{name: "maint plan registry", args: []string{"maint", "plan", "registry"}, want: true},
		{name: "maint plan sizes", args: []string{"maint", "plan", "sizes"}, want: true},
		{name: "maint apply with no target", args: []string{"maint", "apply"}, want: true},
		{name: "maint apply worktree", args: []string{"maint", "apply", "worktree"}, want: true},
		{name: "maint plan baks", args: []string{"maint", "plan", "baks"}, want: true},
		{name: "the maint group alone", args: []string{"maint"}, want: false},
		{name: "an unknown maint leaf", args: []string{"maint", "report"}, want: false},
		{name: "no arguments", args: nil, want: false},
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

func TestNativeGtEntryCannotNameAPathOutsideTheHostSet(t *testing.T) {
	t.Parallel()
	n := Native{Tool: "gt", Path: "/usr/local/bin/gt", Subcommands: []string{"status"}}
	if n.Covers([]string{"status"}) {
		t.Fatal("Covers(status) = true, want false for a gt entry outside the host command set")
	}
}

func TestNativeCoversBdHooksRunOnly(t *testing.T) {
	t.Parallel()
	n := Native{Tool: "bd", Path: "/usr/local/bin/bd", Subcommands: []string{"hooks"}}
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "hooks run", args: []string{"hooks", "run", "pre-commit"}, want: true},
		{name: "hooks list", args: []string{"hooks", "list"}, want: false},
		{name: "hooks alone", args: []string{"hooks"}, want: false},
		{name: "show", args: []string{"show", "x"}, want: false},
		{name: "run named after hooks", args: []string{"run", "hooks"}, want: false},
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

func TestNativeRefusesItself(t *testing.T) {
	t.Parallel()
	// The wrapper is the test binary, a real executable; the other file is the
	// system shell, a different one. Neither is written by the test.
	dir := t.TempDir()
	wrapper, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	const other = "/bin/sh"
	link := filepath.Join(dir, "om-link")
	if err := os.Symlink(wrapper, link); err != nil {
		t.Fatalf("symlink the wrapper: %v", err)
	}
	hard := filepath.Join(dir, "om-hard")
	if err := os.Link(wrapper, hard); err != nil {
		t.Fatalf("link the wrapper: %v", err)
	}
	self := func() (string, error) { return wrapper, nil }

	for name, path := range map[string]string{"the same path": wrapper, "a symlink to it": link, "a hard link of it": hard} {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()
			err := Native{Tool: "om", Path: path}.CheckNotSelf(os.Stat, self)
			if err == nil || !strings.Contains(err.Error(), "native.om.path") || !strings.Contains(err.Error(), "is this wrapper itself") {
				t.Fatalf("CheckNotSelf = %v, want a refusal naming native.om.path and the wrapper", err)
			}
		})
	}
	t.Run("accepts another file", func(t *testing.T) {
		t.Parallel()
		if err := (Native{Tool: "om", Path: other}).CheckNotSelf(os.Stat, self); err != nil {
			t.Fatalf("CheckNotSelf = %v, want nil for a different file", err)
		}
	})
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		executable func() (string, error)
		stat       func(string) (fs.FileInfo, error)
	}{
		"no way to find the wrapper": {executable: nil, stat: os.Stat},
		"the wrapper not found":      {executable: func() (string, error) { return "", boom }, stat: os.Stat},
		"the wrapper unreadable":     {executable: self, stat: func(p string) (fs.FileInfo, error) { return nil, boom }},
	} {
		t.Run("refuses when "+name, func(t *testing.T) {
			t.Parallel()
			err := Native{Tool: "om", Path: other}.CheckNotSelf(tc.stat, tc.executable)
			if err == nil || !strings.Contains(err.Error(), "cannot be compared") {
				t.Fatalf("CheckNotSelf = %v, want a refusal: unknown is not different", err)
			}
		})
	}
}
