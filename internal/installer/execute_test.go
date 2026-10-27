package installer

import (
	"errors"
	"io/fs"
	"os/exec"
	"testing"
)

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
