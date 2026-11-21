package done

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
)

// TestLineLimitWiringRefusesAPolecatPastTwiceTheLimit pins the wiring: a
// polecat's submission reaches the added-line check after the throwaway-file
// check, and a branch past twice the declared limit is refused there before
// reportUnchangedSinceRejection runs (gt-r160a1).
func TestLineLimitWiringRefusesAPolecatPastTwiceTheLimit(t *testing.T) {
	t.Parallel()
	const baseRef = "origin/main"
	var calls []string
	g := git.NewGitWithExec(t.TempDir(), func(c git.ExecCall) (string, string, error) {
		args := c.Args()
		calls = append(calls, strings.Join(args, " "))
		switch {
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--numstat":
			return "141\t0\tinternal/done/done.go\n", "", nil
		case len(args) >= 1 && args[0] == "ls-files":
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected git call: git %s", strings.Join(args, " "))
	})
	var out bytes.Buffer
	r := &doneRun{
		g:    g,
		out:  &out,
		opts: doneOptions{polecatEnv: true, allowReverts: true, allowThrowawayPaths: true},
	}
	// The notes make the later check act if it is reached: it starts by
	// resolving HEAD, which the runner answers with an error.
	sub := doneSubmission{sourceIssue: &beads.Issue{
		Description: "Change: at most 70 added lines in 2 files.",
		Notes:       rejectionNotes("polecat/nux/gt-test", "gt-wisp-1"),
	}}

	err := checkBranchForSubmit(r, sub, baseRef)
	if err == nil {
		t.Fatal("checkBranchForSubmit accepted a polecat branch past twice the declared limit")
	}
	for _, want := range []string{"Change exceeds the 70-added-line constraint", "141 added lines", splitLine} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%s", want, err)
		}
	}
	if !strings.Contains(out.String(), "141 added lines") {
		t.Errorf("the check printed %q, want the polecat's line count", out.String())
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "rev-parse") {
			t.Errorf("the rejected-rework check ran after the added-line refusal: git %s", call)
		}
	}
}

// pastLimitHarness is a submit harness whose git reports 141 added lines
// against the 70 its bead declares, so checkBranchForSubmit reaches the size
// refusal with a fake origin to push to.
func pastLimitHarness(t *testing.T, backend string, polecat bool) *submitHarness {
	t.Helper()
	h := newSubmitHarness(t)
	h.r.sessionBackend = backend
	h.r.opts = doneOptions{polecatEnv: polecat, allowReverts: true, allowThrowawayPaths: true}
	h.r.out = new(bytes.Buffer)
	h.r.g = git.NewGitWithExec(t.TempDir(), func(c git.ExecCall) (string, string, error) {
		if args := c.Args(); len(args) >= 2 && args[0] == "diff" && args[1] == "--numstat" {
			return "141\t0\tinternal/done/done.go\n", "", nil
		}
		return "", "", nil
	})
	return h
}

// TestLineLimitRefusalPushesASandboxSeatsBranch: a sandbox polecat refused for a
// size overrun has its branch pushed to origin before the refusal returns, so the
// commit does not live only in the sandbox that is about to stop; a crew branch is
// warned, not refused, and pushes nothing.
func TestLineLimitRefusalPushesASandboxSeatsBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		backend    string
		polecat    bool
		wantErr    bool
		wantPushed bool
	}{
		{name: "sandbox_polecat", backend: backendOpenShell, polecat: true, wantErr: true, wantPushed: true},
		{name: "crew_branch", backend: backendOpenShell, polecat: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := pastLimitHarness(t, tt.backend, tt.polecat)
			sub := doneSubmission{sourceIssue: &beads.Issue{Description: "Change: at most 70 added lines in 2 files."}}

			err := checkBranchForSubmit(h.r, sub, "origin/main")

			if (err != nil) != tt.wantErr {
				t.Fatalf("checkBranchForSubmit error = %v, want error %v", err, tt.wantErr)
			}
			if pushed := h.repo.origin[doneTestBranch] == h.repo.head; pushed != tt.wantPushed {
				t.Errorf("origin/%s = %q (head %s): pushed = %v, want %v", doneTestBranch, h.repo.origin[doneTestBranch], h.repo.head, pushed, tt.wantPushed)
			}
		})
	}
}
