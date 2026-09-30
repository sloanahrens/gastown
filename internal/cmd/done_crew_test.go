package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// crewSubmittedRE is the landing worker's submission-comment pattern
// (landworker.submittedRE); a crew comment must satisfy it.
var crewSubmittedRE = regexp.MustCompile(`Submitted for landing: (\S+) @ ([0-9a-fA-F]{4,40})(?: onto (\S+))?`)

const (
	crewTestBranch = "crew/sloan/crew-done-submit"
	crewTestHead   = "c0ffee1234abcdef"
)

// newCrewSubmitHarness is the submit harness as a crew run: a crew branch
// already pushed at HEAD, no polecat, no seat.
func newCrewSubmitHarness(t *testing.T) *submitHarness {
	t.Helper()
	h := newSubmitHarness(t)
	h.r.branch = crewTestBranch
	h.r.polecatName = ""
	h.r.sender = "Sloan Ahrens"
	h.r.opts = doneOptions{}
	// A hex head, so the comment is one the worker's pattern can read.
	h.repo.head = crewTestHead
	h.repo.ancestors = map[string]bool{"main1": true, crewTestHead: true}
	h.repo.origin[crewTestBranch] = crewTestHead
	return h
}

// TestCrewSubmitMarksPushedBranchReadyToLand: a crew branch pushed at HEAD
// is gated at HEAD, then the bead gets the worker's submission comment, a
// READY TO LAND block naming exactly that head and the ready label. Nothing
// is pushed, rebased or closed.
func TestCrewSubmitMarksPushedBranchReadyToLand(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	head := crewTestHead
	if len(h.gate.heads) != 1 || h.gate.heads[0] != head {
		t.Errorf("gate ran on %v, want exactly %s", h.gate.heads, head)
	}
	if len(h.repo.pushes) != 0 || len(h.stages) != 0 {
		t.Errorf("crew submit pushed %v / ran stages %v; crew push their own branch", h.repo.pushes, h.stages)
	}
	issue := h.source(t)
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s", issue.Labels, land.LabelReadyToLand)
	}
	if issue.Status == string(beads.StatusClosed) {
		t.Error("crew submit closed the bead; only the landing worker may")
	}
	w, err := land.WorkFromBead(issue, "gastown")
	if err != nil || w.Branch != crewTestBranch || w.Head != head || w.Target != "main" || w.Worker != "" {
		t.Errorf("landing request = %+v (%v) from notes %q", w, err, issue.Notes)
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %v (%v), want one submission comment", comments, err)
	}
	m := crewSubmittedRE.FindStringSubmatch(comments[0].Text)
	if m == nil || m[1] != crewTestBranch || m[2] != head || m[3] != "main" {
		t.Errorf("submission comment %q does not parse as %s @ %s onto main (got %v)", comments[0].Text, crewTestBranch, head, m)
	}
}

// TestCrewSubmitPreVerifiedSkipsGate: --pre-verified skips only the local
// gate; the bead is still marked ready.
func TestCrewSubmitPreVerifiedSkipsGate(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	h.r.opts.preVerified = true
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	if len(h.gate.heads) != 0 {
		t.Errorf("gate ran on %v despite --pre-verified", h.gate.heads)
	}
	if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("pre-verified crew submit did not mark the bead ready")
	}
}

// TestCrewSubmitRefusals: every refusal leaves the bead unmarked.
func TestCrewSubmitRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(h *submitHarness)
		code  int // 0 = plain error
		text  string
	}{
		{"not pushed", func(h *submitHarness) { delete(h.repo.origin, crewTestBranch) }, doneExitPushUnverified, "push it first"},
		{"origin behind HEAD", func(h *submitHarness) { h.repo.origin[crewTestBranch] = "older" }, doneExitPushUnverified, "is not at HEAD"},
		{"gate red", func(h *submitHarness) { h.gate.result = land.GateResult{Passed: false} }, doneExitGateFailed, "local gate failed"},
		{"nothing ahead", func(h *submitHarness) { h.repo.ahead = 0 }, 0, "nothing to land"},
		{"no bead", func(h *submitHarness) { h.r.issueID = "" }, 0, "pass --bead"},
		{"main branch", func(h *submitHarness) { h.r.branch = "main" }, 0, "cannot submit the main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCrewSubmitHarness(t)
			tc.setup(h)
			err := submitCrewForLanding(h.r)
			if err == nil {
				t.Fatal("crew submit succeeded, want a refusal")
			}
			if tc.code != 0 {
				wantDoneExit(t, err, tc.code, tc.text)
			} else if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("error %q lacks %q", err, tc.text)
			}
			issue := h.source(t)
			if beads.HasLabel(issue, land.LabelReadyToLand) || strings.Contains(issue.Notes, land.ReadyNoteMarker) {
				t.Errorf("refused crew submit still marked the bead: labels %v notes %q", issue.Labels, issue.Notes)
			}
		})
	}
}

// TestDoneIsCrewRun: only an identity with no polecat trace and a crew (or
// absent) actor and role takes the crew path.
func TestDoneIsCrewRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, true},
		{map[string]string{"BD_ACTOR": "gastown/crew/sloan"}, true},
		{map[string]string{"BD_ACTOR": "gastown/crew/sloan", "GT_ROLE": "gastown/crew/sloan"}, true},
		{map[string]string{"BD_ACTOR": "gastown/polecats/refuge"}, false},
		{map[string]string{"GT_POLECAT": "refuge"}, false},
		{map[string]string{"GT_ROLE": "gastown/polecats/refuge"}, false},
		{map[string]string{"GT_ROLE": "polecat"}, false},
		{map[string]string{"BD_ACTOR": "gastown/witness"}, false},
		{map[string]string{"GT_ROLE": "mayor"}, false},
	}
	for _, tc := range cases {
		getenv := func(k string) string { return tc.env[k] }
		if got := doneIsCrewRun(getenv); got != tc.want {
			t.Errorf("doneIsCrewRun(%v) = %v, want %v", tc.env, got, tc.want)
		}
	}
}

// TestCrewBeadFromBranch: a slug that merely looks like a bead id is not
// one unless the town routes its prefix.
func TestCrewBeadFromBranch(t *testing.T) {
	t.Parallel()
	routed := func(prefix string) bool { return prefix == "gt-" }
	for branch, want := range map[string]string{
		"crew/sloan/gt-3e7tk-crew-submit": "gt-3e7tk",
		"crew/sloan/crew-done-submit":     "",
		"crew/sloan/fix":                  "",
		"gt-abc.2":                        "gt-abc.2",
	} {
		if got := crewBeadFromBranch(branch, routed); got != want {
			t.Errorf("crewBeadFromBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}

// TestDoneNeedsPolecatWorktree: the pre-run worktree guard applies to a
// polecat's gt done only; a crew gt done reaches runDoneCrew.
func TestDoneNeedsPolecatWorktree(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "gt"}
	done := &cobra.Command{Use: "done"}
	root.AddCommand(done)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if doneNeedsPolecatWorktree(done, env(map[string]string{"BD_ACTOR": "gastown/crew/sloan"})) {
		t.Error("crew gt done hit the polecat worktree guard")
	}
	if doneNeedsPolecatWorktree(done, env(map[string]string{})) {
		t.Error("gt done with no identity hit the polecat worktree guard")
	}
	if !doneNeedsPolecatWorktree(done, env(map[string]string{"BD_ACTOR": "gastown/polecats/refuge", "GT_POLECAT": "refuge"})) {
		t.Error("polecat gt done skipped the worktree guard")
	}
	if doneNeedsPolecatWorktree(root, env(map[string]string{"GT_POLECAT": "refuge"})) {
		t.Error("a command other than gt done hit the worktree guard")
	}
}
