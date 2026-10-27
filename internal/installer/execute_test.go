package installer

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRealExecuteReportsTheStatusBashWould pins the two numbers a command that
// never ran is reported with, so the slot step's 126/127 reading holds: a path
// that is there and not executable is 126, and a name nothing can execute is
// 127. No process runs in either case — the kernel refuses the exec of the
// 0644 file and the lookup finds nothing — which is why this is a test of
// RealExecute at all (gt-tpg0p.11).
func TestRealExecuteReportsTheStatusBashWould(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "gt")
	if err := os.WriteFile(notExecutable, []byte("not a program\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		// command is the name to run.
		command string
		want    int
	}{
		{
			name:    "a path that exists and cannot be executed",
			command: notExecutable,
			want:    126,
		},
		{
			name:    "a path that is not there",
			command: filepath.Join(dir, "missing"),
			want:    127,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := RealExecute("", tc.command, nil)

			if code != tc.want {
				t.Errorf("RealExecute(%q) = %d, want %d (stdout %q, stderr %q)", tc.command, code, tc.want, stdout, stderr)
			}
		})
	}
}

// TestStatusOfReadsAPermissionErrorInsideExecsError pins the mapping of a
// permission denial exec reports inside its own error: the status is still 126,
// "found but could not be executed". os.IsPermission does not look inside that
// error, so with it this would read as 127, "not found" (gt-tpg0p.11).
func TestStatusOfReadsAPermissionErrorInsideExecsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"a permission error inside exec's own error", &exec.Error{Name: "gt", Err: fs.ErrPermission}, 126},
		{"a permission error on a path", &fs.PathError{Op: "fork/exec", Path: "/bin/gt", Err: fs.ErrPermission}, 126},
		{"a command that is not there", &exec.Error{Name: "gt", Err: exec.ErrNotFound}, 127},
		{"an error that names no permission", errors.New("boom"), 127},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statusOf(tc.err); got != tc.want {
				t.Errorf("statusOf(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
