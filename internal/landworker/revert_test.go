package landworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	greenSHA  = "6666666666666666666666666666666666666666"
	redSHA    = "7777777777777777777777777777777777777777"
	revertSHA = "8888888888888888888888888888888888888888"
)

type revertHarness struct {
	*redMainHarness
	state    *MemoryMainState
	files    *fakeLandings
	reverts  []string
	buildErr error
	// changed is what the culprit's landing is reported to have changed;
	// diffErr makes reading it fail, diffCalls counts the reads.
	changed   []string
	diffErr   error
	diffCalls int
}

// newRevertHarness is a red-main owner that last saw main green at greenSHA,
// with gt-cul's landing recorded at redSHA on top of baseOfCulprit. The
// landing changed a Go file, so a Go package that stayed red is its doing.
func newRevertHarness(t *testing.T, baseOfCulprit string) *revertHarness {
	t.Helper()
	h := &revertHarness{redMainHarness: newRedMainHarness(t), state: &MemoryMainState{}, files: &fakeLandings{},
		changed: []string{"internal/a/a.go"}}
	h.r.State, h.r.Landings = h.state, h.files
	h.r.Revert = func(_ context.Context, rec land.LandingRecord, branch string) (string, error) {
		h.reverts = append(h.reverts, rec.LandedCommit+" on "+branch)
		return revertSHA, h.buildErr
	}
	h.r.Diff = func(_ context.Context, _ land.LandingRecord) ([]string, error) {
		h.diffCalls++
		return h.changed, h.diffErr
	}
	h.r.Green(context.Background(), "make test-slow", PostLand{BeadID: "gt-prev", Commit: greenSHA}, PostLandResult{})
	h.bd.Seed(beads.Issue{ID: "gt-cul", Title: "culprit work", Status: "closed", Type: "task", Assignee: "gastown/polecats/opal"})
	h.files.recs = []land.LandingRecord{{BeadID: "gt-cul", Rig: "gastown", Branch: "polecat/opal/gt-cul", Target: "main", Base: baseOfCulprit, LandedCommit: redSHA}}
	h.rerunExit[pkgA] = 1
	return h
}

func (h *revertHarness) red(ctx context.Context, pl PostLand) {
	h.r.Red(ctx, "make test-slow", pl, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
}

// redShell is a red run the post-land shell tier caused: it names no Go
// package, only the script that failed, so nothing is rerun.
func (h *revertHarness) redShell(ctx context.Context, pl PostLand, script string) {
	h.r.Red(ctx, "bash scripts/post-land-shell.sh", pl, PostLandResult{ExitCode: 1, ShellFailures: []string{script}})
}

func (h *revertHarness) revertBeads(t *testing.T) []*beads.Issue {
	t.Helper()
	issues, err := h.bd.List(beads.ListOptions{Label: LabelRevert, Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	return issues
}

func TestRedMainFilesARevertOfTheOnlyLandingSinceGreen(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.red(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"})

	wantBranch := RevertBranch("gt-cul", redSHA)
	if len(h.reverts) != 1 || h.reverts[0] != redSHA+" on "+wantBranch {
		t.Fatalf("reverts built %v; want %s on %s", h.reverts, redSHA, wantBranch)
	}
	rv := h.revertBeads(t)
	if len(rv) != 1 || rv[0].Title != RevertTitle("gastown", "gt-cul") || !beads.HasLabel(rv[0], land.LabelReadyToLand) {
		t.Fatalf("revert beads %+v; want one, ready to land", rv)
	}
	// The worker lands it like any work: the READY TO LAND block says what.
	work, err := land.WorkFromBead(rv[0], "gastown")
	if err != nil || work.Branch != wantBranch || work.Head != revertSHA || work.Target != "main" {
		t.Fatalf("landing request %+v (%v)", work, err)
	}
	if culprit, landed, ok := ParseRevertNote(rv[0].Notes); !ok || culprit != "gt-cul" || landed != redSHA {
		t.Fatalf("revert note %q", rv[0].Notes)
	}
	if s := h.lastStatus(); !strings.HasPrefix(s, "main RED at 77777777 (landed by gt-cul): ") || !strings.HasSuffix(s, "; reverting gt-cul as "+rv[0].ID) {
		t.Fatalf("status %q", s)
	}
	if st, _ := h.state.Load(); st.LastGreen != greenSHA || st.LastRun != redSHA {
		t.Fatalf("state %+v; the red run must not move the last green", st)
	}

	// Red again before the revert lands: no second revert.
	h.red(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"})
	if len(h.reverts) != 1 || len(h.revertBeads(t)) != 1 || !strings.Contains(h.lastStatus(), "revert of gt-cul already open as "+rv[0].ID) {
		t.Fatalf("second red: reverts %v, status %q", h.reverts, h.lastStatus())
	}
}

// gt-40so9: at 4576c678 a Go-only landing (gt-vsct7.1) was reverted for a
// scripts/test-makefile.sh failure it cannot have caused.
func TestRedMainSkipsTheRevertWhenTheLandingCannotHaveFailedTheScript(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.changed = []string{"internal/attention/attention.go", "internal/cmd/attention.go", "docs/reference.md"}
	h.redShell(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"}, "scripts/test-makefile.sh")

	if len(h.reverts) != 0 || len(h.revertBeads(t)) != 0 {
		t.Fatalf("reverted a landing that cannot have failed the script: %v %+v", h.reverts, h.revertBeads(t))
	}
	if s := h.lastStatus(); !strings.Contains(s, "no revert: scripts/test-makefile.sh: the landing changed no shell-tier input (internal/attention/attention.go, internal/cmd/attention.go, docs/reference.md)") {
		t.Fatalf("status %q; want the skipped revert and why", s)
	}
	// The red-main bead stands: the failure is a human's call.
	if open := h.open(t); len(open) != 1 || open[RedMainTitle("gastown", redMainNoPackage)] == nil {
		t.Fatalf("red-main beads %+v; want the whole-command bead", open)
	}
}

func TestRedMainRevertsWhenTheLandingTouchedTheFailingScript(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.changed = []string{"scripts/test-makefile.sh", "scripts/lib/install-gt-lib.sh"}
	h.redShell(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"}, "scripts/test-makefile.sh")

	rv := h.revertBeads(t)
	if len(h.reverts) != 1 || len(rv) != 1 {
		t.Fatalf("reverts %v, beads %+v; want the failing script's landing reverted", h.reverts, rv)
	}
	if s := h.lastStatus(); !strings.Contains(s, "reverting gt-cul as "+rv[0].ID) {
		t.Fatalf("status %q", s)
	}
}

func TestRedMainSkipsTheRevertWhenTheDiffChangedNoGoInput(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.changed = []string{"scripts/lint-lock-wait.sh"}
	h.red(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"})

	if len(h.reverts) != 0 || len(h.revertBeads(t)) != 0 {
		t.Fatalf("reverted a script-only landing for a Go failure: %v %+v", h.reverts, h.revertBeads(t))
	}
	if s := h.lastStatus(); !strings.Contains(s, "no revert: "+pkgA+": the landing changed no Go input (scripts/lint-lock-wait.sh)") {
		t.Fatalf("status %q; want the skipped revert and why", s)
	}
}

// A red run that named no unit (a build the merged tree failed) keeps the
// revert: nothing names what the landing would have to have moved.
func TestRedMainRevertsARedRunThatNamedNoUnit(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.r.Diff = nil
	h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"},
		PostLandResult{ExitCode: 2, Tail: "build failed\n"})

	if len(h.reverts) != 1 || len(h.revertBeads(t)) != 1 {
		t.Fatalf("reverts %v, beads %+v; want the unnamed failure reverted", h.reverts, h.revertBeads(t))
	}
}

// A red run that failed in two units is only the landing's doing when its
// diff can have moved both.
func TestRedMainSkipsTheRevertWhenOneFailingUnitIsUnattributable(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.changed = []string{"internal/a/a.go"}
	h.r.Red(context.Background(), "bash scripts/post-land-shell.sh", PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"},
		PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false}), ShellFailures: []string{"scripts/test-makefile.sh"}})

	if len(h.reverts) != 0 || len(h.revertBeads(t)) != 0 {
		t.Fatalf("reverted for a unit the diff cannot have moved: %v %+v", h.reverts, h.revertBeads(t))
	}
	if s := h.lastStatus(); !strings.Contains(s, "scripts/test-makefile.sh: the landing changed no shell-tier input") {
		t.Fatalf("status %q; want the script it stopped at", s)
	}
}

// The cancellation guard comes before the diff read (gt-40so9): a red run the
// daemon stopped answers "nobody's revert yet" without spending a git read on
// an attribution it will not act on. A shell failure reaches here whatever the
// context says — nothing is rerun, so nothing checks the cancellation first.
func TestRedMainSkipsTheDiffReadWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.changed = []string{"scripts/test-makefile.sh"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.redShell(ctx, PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"}, "scripts/test-makefile.sh")

	if h.diffCalls != 0 {
		t.Fatalf("read the landing's diff %d time(s) on a cancelled context", h.diffCalls)
	}
	if len(h.reverts) != 0 {
		t.Fatalf("built a revert on a cancelled context: %v", h.reverts)
	}
}

func TestRedMainDoesNotRevert(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		base       string
		pl         PostLand
		setup      func(h *revertHarness)
		wantStatus string
	}{
		{name: "more than one landing since green", base: "5555555555", pl: PostLand{BeadID: "gt-cul", Commit: redSHA},
			wantStatus: "no revert: more than one change since the last green 66666666"},
		{name: "direct push", base: greenSHA, pl: PostLand{Commit: redSHA, Direct: true, From: greenSHA},
			wantStatus: "main RED at 77777777 (direct push 66666666..77777777): "},
		{name: "no green recorded", base: greenSHA, pl: PostLand{BeadID: "gt-cul", Commit: redSHA},
			setup:      func(h *revertHarness) { _ = h.state.Save(MainState{}) },
			wantStatus: "no revert: no green commit recorded yet"},
		{name: "culprit is a revert", base: greenSHA, pl: PostLand{BeadID: "gt-cul", Commit: redSHA},
			setup: func(h *revertHarness) {
				_ = h.bd.Update("gt-cul", beads.UpdateOptions{AddLabels: []string{LabelRevert}})
			},
			wantStatus: "no revert: the culprit is itself a revert"},
		{name: "build fails", base: greenSHA, pl: PostLand{BeadID: "gt-cul", Commit: redSHA},
			setup:      func(h *revertHarness) { h.buildErr = errors.New("revert conflicts") },
			wantStatus: "no revert: building it failed (revert conflicts)"},
		{name: "the landing's diff is unreadable", base: greenSHA, pl: PostLand{BeadID: "gt-cul", Commit: redSHA},
			setup:      func(h *revertHarness) { h.diffErr = errors.New("fatal: bad revision") },
			wantStatus: "no revert: the landing's changed paths are unreadable"},
		{name: "no way to read the landing's diff", base: greenSHA, pl: PostLand{BeadID: "gt-cul", Commit: redSHA},
			setup:      func(h *revertHarness) { h.r.Diff = nil },
			wantStatus: "no revert: the landing's changed paths are unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRevertHarness(t, tc.base)
			if tc.setup != nil {
				tc.setup(h)
			}
			h.red(context.Background(), tc.pl)
			if len(h.revertBeads(t)) != 0 {
				t.Fatalf("filed a revert: %+v", h.revertBeads(t))
			}
			if s := h.lastStatus(); !strings.Contains(s, tc.wantStatus) {
				t.Fatalf("status %q; want it to contain %q", s, tc.wantStatus)
			}
			// The red-main bead stands either way.
			if len(h.open(t)) != 1 {
				t.Fatalf("red-main beads %v", h.open(t))
			}
			// And no revert is in flight, so a dispatcher may take it.
			if st, _ := h.state.Load(); st.Revert != nil {
				t.Fatalf("state %+v; the owner reported no revert", st)
			}
		})
	}
}

func TestRedMainDirectPushNamesTheRange(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	h.red(context.Background(), PostLand{Commit: redSHA, Direct: true, From: greenSHA})
	a := h.open(t)[RedMainTitle("gastown", pkgA)]
	if a == nil || !strings.Contains(a.Description, "suspect range "+greenSHA+".."+redSHA) {
		t.Fatalf("red-main bead %+v; want the direct push's range", a)
	}
	// A green direct push moves the last green, so the next landing on it
	// can be reverted.
	h.r.Green(context.Background(), "make test-slow", PostLand{Commit: "9999", Direct: true, From: redSHA}, PostLandResult{})
	if st, _ := h.state.Load(); st.LastGreen != "9999" {
		t.Fatalf("state %+v", st)
	}
}

// gt-zkdwt: a dispatcher sent a polecat after a red-main bead the owner was
// already undoing, because nothing it could read said a revert was on its way.
func TestRedMainRecordsTheRevertItHasInFlight(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	if st, _ := h.state.Load(); st.Revert != nil {
		t.Fatalf("state %+v; a green main has no revert in flight", st)
	}
	h.red(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"})
	rv := h.revertBeads(t)
	if len(rv) != 1 {
		t.Fatalf("revert beads %+v", rv)
	}
	if st, _ := h.state.Load(); st.Revert == nil || st.Revert.Culprit != "gt-cul" || st.Revert.Bead != rv[0].ID {
		t.Fatalf("state %+v; want the revert of gt-cul in flight as %s", st, rv[0].ID)
	} else if st.Revert.StartedAt.IsZero() {
		t.Fatalf("state %+v; want the build's start time recorded so the dispatcher can expire it (gt-wgyca)", st.Revert)
	}
}

func TestRedMainRecordsARevertItIsStillBuilding(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	// The build reads the state back: it must already say the revert is
	// coming, or a dispatcher reading it mid-build sees a fix-forward seat.
	var seen *PendingRevert
	h.r.Revert = func(_ context.Context, rec land.LandingRecord, _ string) (string, error) {
		st, _ := h.state.Load()
		seen = st.Revert
		return revertSHA, nil
	}
	h.red(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"})
	if seen == nil || seen.Culprit != "gt-cul" || seen.Bead != "" {
		t.Fatalf("state during the build %+v; want the culprit with no bead yet", seen)
	}
	if seen.StartedAt.IsZero() {
		t.Fatalf("state during the build %+v; want the build's start time recorded", seen)
	}
	// Filing the bead continues that build, so the start time it was recorded
	// with is the one the dispatcher still counts from (gt-wgyca).
	if st, _ := h.state.Load(); st.Revert == nil || !st.Revert.StartedAt.Equal(seen.StartedAt) {
		t.Fatalf("state after filing %+v; want the build's start time %v kept", st.Revert, seen.StartedAt)
	}
}

func TestRedMainDoesNotRecordTheRevertsItDidNotFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(h *revertHarness)
	}{
		{name: "more than one change since green", setup: func(h *revertHarness) {
			h.files.recs[0].Base = "5555555555"
		}},
		{name: "the landing's diff is unreadable", setup: func(h *revertHarness) { h.diffErr = errors.New("fatal: bad revision") }},
		{name: "the build fails", setup: func(h *revertHarness) { h.buildErr = errors.New("revert conflicts") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRevertHarness(t, greenSHA)
			tc.setup(h)
			h.red(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA, Target: "main"})
			if len(h.revertBeads(t)) != 0 {
				t.Fatalf("filed a revert: %+v", h.revertBeads(t))
			}
			if st, _ := h.state.Load(); st.Revert != nil {
				t.Fatalf("state %+v; no revert was filed, so none is in flight", st)
			}
		})
	}
}

func TestRevertLandedClearsTheRevertInFlight(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	id := seedRevertBead(t, h)
	h.r.RevertLanded(context.Background(), land.Work{BeadID: id}, land.Result{LandedCommit: "abababab"})
	if st, _ := h.state.Load(); st.Revert != nil {
		t.Fatalf("state %+v; a landed revert is no longer in flight", st)
	}
}

func TestRevertRejectedClearsTheRevertInFlight(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	id := seedRevertBead(t, h)
	h.r.RevertRejected(land.Work{BeadID: id}, &land.Rejection{Kind: land.RejectGate, Reason: "gate failed"})
	if st, _ := h.state.Load(); st.Revert != nil {
		t.Fatalf("state %+v; a rejected revert is no longer in flight", st)
	}
}

func seedRevertBead(t *testing.T, h *revertHarness) string {
	t.Helper()
	h.red(context.Background(), PostLand{BeadID: "gt-cul", Commit: redSHA})
	rv := h.revertBeads(t)
	if len(rv) != 1 {
		t.Fatalf("revert beads %+v", rv)
	}
	return rv[0].ID
}

func TestRevertLandedReopensTheCulpritForRework(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	id := seedRevertBead(t, h)
	h.r.RevertLanded(context.Background(), land.Work{BeadID: id}, land.Result{LandedCommit: "abababab"})

	cul, _ := h.bd.Show("gt-cul")
	if cul.Status != string(beads.StatusOpen) || cul.Assignee != "" || !beads.HasLabel(cul, land.LabelRework) {
		t.Fatalf("culprit %+v; want open, unassigned, rework", cul)
	}
	cs, _ := h.bd.Comments("gt-cul")
	if len(cs) != 1 || !strings.Contains(cs[0].Text, "Reverted by "+id) || !strings.Contains(cs[0].Text, "git revert abababab") {
		t.Fatalf("culprit comments %+v", cs)
	}
	if s := h.lastStatus(); !strings.Contains(s, "reverted gt-cul's landing 77777777") {
		t.Fatalf("status %q", s)
	}

	// Any other landing is not a revert.
	h.bd.Seed(beads.Issue{ID: "gt-other", Title: "work", Status: "closed", Labels: []string{LabelRevert}, Notes: FormatRevertNote(land.LandingRecord{BeadID: "gt-x", LandedCommit: "x"})})
	h.r.RevertLanded(context.Background(), land.Work{BeadID: "gt-other"}, land.Result{LandedCommit: "cd"})
	if x, err := h.bd.Show("gt-x"); err == nil && x != nil {
		t.Fatalf("a bead titled unlike a revert reopened %+v", x)
	}
	if cs, _ := h.bd.Comments("gt-cul"); len(cs) != 1 {
		t.Fatalf("culprit commented twice: %+v", cs)
	}
}

func TestRevertRejectedClosesTheRevertInsteadOfRework(t *testing.T) {
	t.Parallel()
	h := newRevertHarness(t, greenSHA)
	id := seedRevertBead(t, h)
	_ = h.bd.Update(id, beads.UpdateOptions{AddLabels: []string{land.LabelRework}, RemoveLabels: []string{land.LabelReadyToLand}})
	if !h.r.RevertRejected(land.Work{BeadID: id}, &land.Rejection{Kind: land.RejectConflict, Reason: "conflicts in 2 files", Rework: true}) {
		t.Fatal("a revert's rejection was not handled")
	}
	rv, _ := h.bd.Show(id)
	if rv.Status != string(beads.StatusClosed) || beads.HasLabel(rv, land.LabelRework) || !strings.Contains(rv.CloseReason, "red-main beads stand") {
		t.Fatalf("revert bead %+v; want closed without rework", rv)
	}
	if cul, _ := h.bd.Show("gt-cul"); cul.Status != "closed" {
		t.Fatalf("culprit reopened by a failed revert: %+v", cul)
	}
	if h.r.RevertRejected(land.Work{BeadID: "gt-cul"}, &land.Rejection{Kind: land.RejectGate}) {
		t.Fatal("ordinary work treated as a revert")
	}
}

type fakeRevertHooks struct {
	landed   []string
	rejected []string
	isRevert bool
}

func (f *fakeRevertHooks) RevertLanded(_ context.Context, w land.Work, res land.Result) {
	f.landed = append(f.landed, w.BeadID+"@"+res.LandedCommit)
}

func (f *fakeRevertHooks) RevertRejected(w land.Work, _ *land.Rejection) bool {
	f.rejected = append(f.rejected, w.BeadID)
	return f.isRevert
}

func TestPassTellsTheRevertHooks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	hooks := &fakeRevertHooks{isRevert: true}
	h.w.Reverts = hooks
	h.seedReady(t, "gt-abc")
	h.seedReady(t, "gt-rej")
	h.lander.fn = func(_ int, w land.Work) (land.Result, error) {
		if w.BeadID == "gt-rej" {
			return land.Result{}, &land.Rejection{Kind: land.RejectGate, Reason: "red", Rework: true}
		}
		return land.Result{LandedCommit: "landed1"}, nil
	}
	h.w.Pass(context.Background())
	if strings.Join(hooks.landed, ",") != "gt-abc@landed1" || strings.Join(hooks.rejected, ",") != "gt-rej" {
		t.Fatalf("landed %v rejected %v", hooks.landed, hooks.rejected)
	}
	if cs := h.comments(t, "gt-rej"); len(cs) != 0 {
		t.Fatalf("a revert's rejection got the rework comment: %q", cs)
	}
}

func TestPassRunsThePostLandCommandForADirectPush(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	trig := &recordTrigger{}
	h.w.PostLand, h.w.WatchTarget = trig, "main"
	ctx := context.Background()

	// Nothing tested yet: the first tip seen is where watching starts.
	h.remote.tips["main"] = "m1"
	h.w.Pass(ctx)
	if len(trig.got) != 0 {
		t.Fatalf("triggered on the first tip: %+v", trig.got)
	}
	h.remote.tips["main"] = "m2"
	h.w.Pass(ctx)
	h.w.Pass(ctx)
	if len(trig.got) != 1 || trig.got[0] != (PostLand{Commit: "m2", Target: "main", Direct: true, From: "m1"}) {
		t.Fatalf("triggers %+v; want one direct run at m2 from m1", trig.got)
	}

	// The worker's own landing moves the tip without a direct run.
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		h.remote.tips["main"] = "landed1"
		return land.Result{LandedCommit: "landed1"}, nil
	}
	h.w.Pass(ctx)
	if len(trig.got) != 2 || trig.got[1].Direct || trig.got[1].Commit != "landed1" {
		t.Fatalf("triggers %+v; want the landing's own run only", trig.got)
	}
}

func TestPassDirectPushWatchSeedsFromTheLastRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	trig := &recordTrigger{}
	h.w.PostLand, h.w.WatchTarget = trig, "main"
	h.w.MainState = &MemoryMainState{st: MainState{LastRun: "tested"}}
	h.remote.tips["main"] = "pushed"
	h.w.Pass(context.Background())
	if len(trig.got) != 1 || trig.got[0].From != "tested" || trig.got[0].Commit != "pushed" || !trig.got[0].Direct {
		t.Fatalf("triggers %+v; a push made while the daemon was down must run", trig.got)
	}

	// A tip the landings file records (landed before a restart) is no push.
	h2 := newHarness(t)
	trig2 := &recordTrigger{}
	h2.w.PostLand, h2.w.WatchTarget = trig2, "main"
	h2.files.recs = []land.LandingRecord{{BeadID: "gt-old", Target: "main", LandedCommit: "l0"}, {BeadID: "gt-new", Target: "main", LandedCommit: "l1"}}
	h2.remote.tips["main"] = "l1"
	h2.w.Pass(context.Background())
	h2.remote.tips["main"] = "l2"
	h2.files.recs = append(h2.files.recs, land.LandingRecord{BeadID: "gt-l2", Target: "main", LandedCommit: "l2"})
	h2.w.Pass(context.Background())
	if len(trig2.got) != 0 {
		t.Fatalf("triggers %+v; landings are not direct pushes", trig2.got)
	}
}

// gt-gb4ij: a daemon restart kills the in-flight run; the first pass after
// the start runs the landing again when its verdict was never reached.
func TestPassResumesALandingRunARestartCutShort(t *testing.T) {
	t.Parallel()
	recs := []land.LandingRecord{{BeadID: "gt-old", Target: "main", LandedCommit: "l0"}, {BeadID: "gt-new", Target: "main", LandedCommit: "l1"}}
	h := newHarness(t)
	trig := &recordTrigger{}
	h.w.PostLand, h.w.WatchTarget = trig, "main"
	h.w.MainState = &MemoryMainState{st: MainState{LastRun: "l0"}}
	h.files.recs = recs
	h.remote.tips["main"] = "l1"
	h.w.Pass(context.Background())
	h.w.Pass(context.Background())
	if len(trig.got) != 1 || trig.got[0] != (PostLand{BeadID: "gt-new", Commit: "l1", Target: "main"}) {
		t.Fatalf("triggers %+v; want one landing run at l1 for gt-new", trig.got)
	}

	// The landing's verdict was reached before the restart: nothing to resume.
	h2 := newHarness(t)
	trig2 := &recordTrigger{}
	h2.w.PostLand, h2.w.WatchTarget = trig2, "main"
	h2.w.MainState = &MemoryMainState{st: MainState{LastRun: "l1"}}
	h2.files.recs = recs
	h2.remote.tips["main"] = "l1"
	h2.w.Pass(context.Background())
	if len(trig2.got) != 0 {
		t.Fatalf("triggers %+v; l1 was already tested", trig2.got)
	}
}

func TestPostLandDirectRedCommentsOnNoBead(t *testing.T) {
	t.Parallel()
	comments := &commentLog{}
	var red []PostLand
	p := &PostLandRunner{Rig: "gastown", Command: func() string { return "make test-slow" }, Beads: comments, Logf: t.Logf,
		Run:   func(context.Context, string, PostLand) PostLandResult { return PostLandResult{ExitCode: 1} },
		OnRed: func(_ context.Context, _ string, pl PostLand, _ PostLandResult) { red = append(red, pl) }}
	p.Trigger(context.Background(), PostLand{Commit: "d1", Direct: true, From: "d0"})
	p.Wait()
	if len(comments.get("")) != 0 || len(red) != 1 {
		t.Fatalf("comments %v red %v", comments.get(""), red)
	}
}

func TestParseRevertNoteReadsTheLastBlock(t *testing.T) {
	t.Parallel()
	notes := FormatRevertNote(land.LandingRecord{BeadID: "gt-a", LandedCommit: "c1"}) + "\n\n" +
		FormatRevertNote(land.LandingRecord{BeadID: "gt-b\nculprit: gt-forged", LandedCommit: "c2"})
	culprit, landed, ok := ParseRevertNote(notes)
	if !ok || culprit != "gt-b culprit: gt-forged" || landed != "c2" {
		t.Fatalf("parsed %q %q %v", culprit, landed, ok)
	}
	if _, _, ok := ParseRevertNote("REVERT OF\nculprit: gt-a"); ok {
		t.Fatal("a block without the landed commit parsed")
	}
}
