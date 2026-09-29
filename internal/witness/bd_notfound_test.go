package witness

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestIsBdNotFoundError: only bd saying the bead does not exist is a
// confirmed absence. A missing bd binary, a missing Dolt database or table,
// or a DNS failure is an outage (G5-02, gt-udrrw).
func TestIsBdNotFoundError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("Error: issue gt-md4z not found"), true},
		{errors.New("Issue gt-x not found\nHint: this ID may have never existed"), true},
		{&beads.CLIError{Args: []string{"show", "gt-x"}, Stderr: []byte("Issue gt-x not found"), Err: exitStatus(1)}, true},
		{&beads.CLIError{Args: []string{"show", "gt-x"}, Err: exitStatus(20)}, true},
		{beads.ErrNotFound, true},
		{errors.New(`exec: "bd": executable file not found in $PATH`), false},
		{errors.New("Error: database not found: gastown"), false},
		{errors.New("Error: table not found: wisps"), false},
		{errors.New("dial tcp: lookup dolt: no such host"), false},
		{errors.New("open /x/.beads: no such file or directory"), false},
		{&beads.CLIError{Args: []string{"show", "gt-x"}, Stderr: []byte("Error: database not found: gastown"), Err: exitStatus(1)}, false},
		{&beads.CLIError{Args: []string{"show", "gt-x"}, Stderr: []byte("Issue gt-x not found"), Err: exitStatus(25)}, false},
		{nil, false},
	} {
		if got := isBdNotFoundError(tc.err); got != tc.want {
			t.Errorf("isBdNotFoundError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// TestFindMRBeadForBranchIsNotTruncated: bd query caps at 50 rows unless told
// otherwise, and a branch's MR past row 50 read as "no MR" (B1-04).
func TestFindMRBeadForBranchIsNotTruncated(t *testing.T) {
	t.Parallel()
	bd, calls := mockBd(
		func(args []string) (string, error) { return "[]", nil },
		func(args []string) error { return nil },
	)
	findMRBeadForBranch(bd, t.TempDir(), "polecat/nux-abc")
	if len(calls.calls) != 1 || !strings.Contains(calls.calls[0], "--limit 0") {
		t.Fatalf("bd calls = %q, want one query with --limit 0", calls.calls)
	}
}
