package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// failedPreserve is a preserve step whose git cannot run at all: no remote, no
// repository, nothing pushed.
type failedPreserve struct{ err error }

func (f failedPreserve) PreserveBranch(string, string, string) (git.PreserveResult, error) {
	return git.PreserveResult{}, f.err
}

// A preserve that could not put the tip on the remote is the case the nuke
// blocks on, and git's sentinel is how it is recognised: the wrapper keeps the
// sentinel and the cause, so the operator sees both the verdict and what git
// said.
func TestPreserveBranchBeforeNukeReportsAPushThatDidNotPreserve(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("%w: push rejected", git.ErrBranchNotPreserved)

	_, err := preserveBranchBeforeNuke(failedPreserve{err: cause}, "polecat/flint/gt-1", "origin", t.TempDir())
	if !errors.Is(err, git.ErrBranchNotPreserved) {
		t.Fatalf("err = %v, want it to wrap git.ErrBranchNotPreserved", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to keep the cause in the chain", err)
	}
}

// A preserve that failed before it could push says nothing about the remote.
// The nuke must still stop — anything but a confirmed push does — but the error
// must not claim the branch is absent from the remote when no push happened.
func TestPreserveBranchBeforeNukeKeepsAnErrorFromBeforeThePush(t *testing.T) {
	t.Parallel()
	cause := errors.New("git rev-parse: fatal: not a git repository")

	_, err := preserveBranchBeforeNuke(failedPreserve{err: cause}, "polecat/flint/gt-1", "origin", t.TempDir())
	if err == nil {
		t.Fatal("err = nil, want the preserve's failure: the nuke must not proceed")
	}
	if errors.Is(err, git.ErrBranchNotPreserved) {
		t.Fatalf("err = %v, want it NOT to claim the branch is not preserved on the remote", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to keep the cause in the chain", err)
	}
}

func TestPreserveFailureBlockerRequiresForceAndAcknowledgement(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("%w: push rejected", git.ErrBranchNotPreserved)

	cases := []struct {
		name        string
		cause       error
		force, ack  bool
		wantBlocked bool
	}{
		{"preserved branch never blocks", nil, false, false, false},
		{"unpreserved, no override", cause, false, false, true},
		{"unpreserved, --force alone", cause, true, false, true},
		{"unpreserved, acknowledgement alone", cause, false, true, true},
		{"unpreserved, --force plus acknowledgement", cause, true, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preserveFailureBlocker("gastown", "flint", tc.force, tc.ack, tc.cause)
			if (err != nil) != tc.wantBlocked {
				t.Fatalf("blocked = %v (err %v), want %v", err != nil, err, tc.wantBlocked)
			}
			if err == nil {
				return
			}
			// The operator has to be able to see both what happened and how to
			// get out of it, without re-reading the source.
			for _, want := range []string{NukeAcknowledgeUnpreservedFlag, "NOT deleted"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err, want)
				}
			}
		})
	}
}
