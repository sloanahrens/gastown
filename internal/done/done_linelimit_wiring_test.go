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
