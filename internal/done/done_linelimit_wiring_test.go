package done

import (
	"bytes"
	"errors"
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
// against the 70 its bead declares (or cannot count them), so checkBranchForSubmit
// reaches the size check with a fake origin to push to.
func pastLimitHarness(t *testing.T, backend string, polecat, countFails bool) *submitHarness {
	t.Helper()
	h := newSubmitHarness(t)
	h.r.sessionBackend = backend
	h.r.opts = doneOptions{polecatEnv: polecat, allowReverts: true, allowThrowawayPaths: true}
	h.r.out = new(bytes.Buffer)
	h.r.g = git.NewGitWithExec(t.TempDir(), func(c git.ExecCall) (string, string, error) {
		if args := c.Args(); len(args) >= 2 && args[0] == "diff" && args[1] == "--numstat" {
			if countFails {
				return "", "", errors.New("numstat failed")
			}
			return "141\t0\tinternal/done/done.go\n", "", nil
		}
		return "", "", nil
	})
	return h
}

// TestLineLimitRefusalPushesASandboxSeatsBranch: a sandbox polecat refused for a
// size overrun has its branch pushed once before the refusal returns, so the
// commit does not live only in the sandbox that is about to stop; a failed push
// warns and the refusal still returns; a crew branch past the limit is warned,
// not refused, and pushes nothing; an added-line count that cannot be read is an
// error for polecat and crew alike and pushes nothing.
func TestLineLimitRefusalPushesASandboxSeatsBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		polecat    bool
		countFails bool
		pushErr    error
		wantErr    string
		wantPushes int
		wantWarn   string
	}{
		{name: "sandbox_polecat_refused", polecat: true, wantErr: splitLine, wantPushes: 1},
		{name: "sandbox_polecat_push_fails", polecat: true, pushErr: errors.New("connection reset"), wantErr: splitLine, wantWarn: "could not push " + doneTestBranch},
		{name: "sandbox_crew_past_limit", polecat: false},
		{name: "sandbox_polecat_count_fails", polecat: true, countFails: true, wantErr: "added-line check"},
		{name: "sandbox_crew_count_fails", polecat: false, countFails: true, wantErr: "added-line check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := pastLimitHarness(t, backendOpenShell, tt.polecat, tt.countFails)
			if tt.pushErr != nil {
				h.repo.pushErrs = []error{tt.pushErr, tt.pushErr}
			}
			sub := doneSubmission{sourceIssue: &beads.Issue{Description: "Change: at most 70 added lines in 2 files."}}

			err := checkBranchForSubmit(h.r, sub, "origin/main")

			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("checkBranchForSubmit error = %v, want one containing %q", err, tt.wantErr)
			}
			if tt.wantPushes == 1 && h.repo.origin[doneTestBranch] != h.repo.head {
				t.Errorf("origin/%s = %q, want head %s", doneTestBranch, h.repo.origin[doneTestBranch], h.repo.head)
			}
			if len(h.repo.pushes) != tt.wantPushes && tt.pushErr == nil {
				t.Errorf("pushes = %v, want %d", h.repo.pushes, tt.wantPushes)
			}
			if out := h.r.out.(*bytes.Buffer).String(); !strings.Contains(out, tt.wantWarn) {
				t.Errorf("output %q does not carry %q", out, tt.wantWarn)
			}
		})
	}
}

// TestLineLimitRefusalNamesThePushedSha: a sandbox polecat past twice the
// declared limit ends its refusal with the short sha its push left on origin and
// the branch; a failed push, and a bead that already landed (its push does not
// run), add no such line, so the refusal ends with the split line (gt-s0kxmj).
func TestLineLimitRefusalNamesThePushedSha(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		pushErr    error
		landed     bool
		pushedLine bool
	}{
		{name: "push_succeeds", pushedLine: true},
		{name: "push_fails", pushErr: errors.New("connection reset")},
		{name: "bead_already_landed", landed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := pastLimitHarness(t, backendOpenShell, true, false)
			if tt.pushErr != nil {
				h.repo.pushErrs = []error{tt.pushErr, tt.pushErr}
			}
			if tt.landed {
				h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusHooked), Notes: alreadyLandedNotes})
			}
			sub := doneSubmission{sourceIssue: &beads.Issue{Description: "Change: at most 70 added lines in 2 files."}}

			err := checkBranchForSubmit(h.r, sub, "origin/main")

			var refusal sizeRefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("checkBranchForSubmit error = %v, want the size refusal", err)
			}
			lines := strings.Split(err.Error(), "\n")
			want := splitLine
			if tt.pushedLine {
				want = "your work is pushed at " + ShortSHA(h.repo.head) + " on " + doneTestBranch
			}
			if last := lines[len(lines)-1]; last != want {
				t.Errorf("last line = %q, want %q", last, want)
			}
		})
	}
}
