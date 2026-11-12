package done

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// absentCrewRun is a crew submission whose worktree is not in this process:
// the run's directory is named but never created, and the repo is the
// harness's fake of the rig's canonical clone, which holds origin's tip.
func absentCrewRun(t *testing.T) (*submitHarness, *bytes.Buffer) {
	t.Helper()
	h := newCrewSubmitHarness(t)
	h.r.cwd = filepath.Join(t.TempDir(), "absent-worktree")
	var out bytes.Buffer
	h.r.out = &out
	return h, &out
}

// TestCrewDoneAbsentWorktreeUsesFlags: with the worktree absent, a branch
// origin has at the flagged head gets the worker's submission comment, the
// READY TO LAND block and the label, exactly as a gated submission does. No
// gate runs, and the output says the presubmit did not run.
func TestCrewDoneAbsentWorktreeUsesFlags(t *testing.T) {
	t.Parallel()
	h, out := absentCrewRun(t)
	if err := submitCrewAbsentForLanding(h.r, crewTestHead); err != nil {
		t.Fatalf("absent crew submit: %v", err)
	}
	if len(h.gate.heads) != 0 {
		t.Errorf("gate ran on %v; the worktree is not in this process to gate", h.gate.heads)
	}
	issue := h.source(t)
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s", issue.Labels, land.LabelReadyToLand)
	}
	w, err := land.WorkFromBead(issue, "gastown")
	if err != nil || w.Branch != crewTestBranch || w.Head != crewTestHead || w.Target != "main" {
		t.Errorf("landing request = %+v (%v), want %s @ %s onto main", w, err, crewTestBranch, crewTestHead)
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %v (%v), want one submission comment", comments, err)
	}
	m := crewSubmittedRE.FindStringSubmatch(comments[0].Text)
	if m == nil || m[1] != crewTestBranch || m[2] != crewTestHead || m[3] != "main" {
		t.Errorf("submission comment %q does not parse as %s @ %s onto main", comments[0].Text, crewTestBranch, crewTestHead)
	}
	const notRun = "presubmit not run: the worktree is not in this process; run make presubmit on the Mac first"
	if !strings.Contains(out.String(), notRun) {
		t.Errorf("output %q lacks %q", out.String(), notRun)
	}
}

// TestCrewDoneAbsentWorktreeRefusesWhenRemoteBehind: origin at another commit,
// or with no such branch, is refused with exit 11 and marks nothing.
func TestCrewDoneAbsentWorktreeRefusesWhenRemoteBehind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tip  string
	}{
		{"remote at another commit", "older"},
		{"remote branch missing", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := absentCrewRun(t)
			h.repo.origin[crewTestBranch] = tc.tip
			err := submitCrewAbsentForLanding(h.r, crewTestHead)
			var exit *ExitCodeError
			if !errors.As(err, &exit) || exit.Code != doneExitPushUnverified {
				t.Fatalf("err = %v, want exit %d", err, doneExitPushUnverified)
			}
			comments, cerr := h.bd.Comments("bd-source")
			if cerr != nil || len(comments) != 0 {
				t.Errorf("comments = %v (%v); a refused submission records nothing", comments, cerr)
			}
			if beads.HasLabel(h.source(t), land.LabelReadyToLand) {
				t.Error("refused submission marked the bead ready to land")
			}
		})
	}
}

// TestCrewFlagFacts: --branch and --head are both or neither, the branch is a
// ref name that cannot read as an option, and the head is a full lowercase
// commit id, so nothing malformed reaches git.
func TestCrewFlagFacts(t *testing.T) {
	t.Parallel()
	full := strings.Repeat("a1", 20)
	branch := "crew/operator/gt-1"
	for _, tc := range []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"sha1 head", Options{Branch: branch, Head: full}, false},
		{"sha256 head", Options{Branch: branch, Head: strings.Repeat("b", 64)}, false},
		{"branch only", Options{Branch: branch}, true},
		{"head only", Options{Head: full}, true},
		{"short head", Options{Branch: branch, Head: "c0ffee"}, true},
		{"uppercase head", Options{Branch: branch, Head: strings.ToUpper(full)}, true},
		{"non-hex head", Options{Branch: branch, Head: strings.Repeat("g", 40)}, true},
		{"branch reads as option", Options{Branch: "--upload-pack=x", Head: full}, true},
		{"branch with space", Options{Branch: "crew/operator x", Head: full}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotBranch, gotHead, err := crewFlagFacts(tc.opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("crewFlagFacts(%+v) err = %v, want error %v", tc.opts, err, tc.wantErr)
			}
			if err == nil && (gotBranch != branch || gotHead != tc.opts.Head) {
				t.Errorf("crewFlagFacts = %q, %q; want %q, %q", gotBranch, gotHead, branch, tc.opts.Head)
			}
		})
	}
}

// TestDoneFlagsRefuseNonCrewSession: a session that is not crew cannot submit
// by flags, whatever directory it names; the refusal comes before any read.
func TestDoneFlagsRefuseNonCrewSession(t *testing.T) {
	t.Parallel()
	err := Run(Options{
		Status: ExitCompleted,
		Branch: crewTestBranch,
		Head:   strings.Repeat("a1", 20),
		Env:    func(string) string { return "" },
		Getwd:  func() (string, error) { return filepath.Join(t.TempDir(), "absent"), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "for crew submissions") {
		t.Fatalf("Run with flags from a non-crew session: err = %v, want the crew-submissions refusal", err)
	}
}
