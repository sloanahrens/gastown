package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// fakeWorkReleaser is an in-memory polecatWorkReleaser: bead -> (status, assignee).
//
// It models bd 1.2.2's write fences: a plain (unguarded) assignee write on an
// in_progress bead someone holds fails (AssigneeNotStolen), and an
// --if-assignee write whose precondition no longer holds writes nothing
// (exit 13 -> released=false, nil). raceTo re-assigns the bead to another
// agent right after the first read, to model a concurrent re-sling.
type fakeWorkReleaser struct {
	beads      map[string][2]string
	readErr    error
	raceTo     string
	restoreErr error
	resetErr   error
	released   []string
	restored   []string
	resets     []string
	annotated  map[string]string
}

func (f *fakeWorkReleaser) HookState(beadID string) (string, string, error) {
	if f.readErr != nil {
		return "", "", f.readErr
	}
	b, ok := f.beads[beadID]
	if !ok {
		return "", "", errors.New("not found")
	}
	if f.raceTo != "" {
		f.beads[beadID] = [2]string{b[0], f.raceTo}
	}
	return b[0], b[1], nil
}

func (f *fakeWorkReleaser) ReleaseBead(beadID, expectedAssignee string) (bool, error) {
	cur := f.beads[beadID]
	if expectedAssignee == "" {
		if cur[0] == "in_progress" && cur[1] != "" {
			return false, errors.New("AssigneeNotStolen: in_progress bead is held by " + cur[1])
		}
	} else if cur[1] != expectedAssignee {
		return false, nil // exit 13: guard no longer held, nothing written
	}
	f.released = append(f.released, beadID)
	f.beads[beadID] = [2]string{"open", ""}
	return true, nil
}

func (f *fakeWorkReleaser) RestoreBead(beadID, expected, status, assignee string) (bool, error) {
	if f.restoreErr != nil {
		return false, f.restoreErr
	}
	if f.beads[beadID][1] != expected {
		return false, nil
	}
	f.restored = append(f.restored, beadID)
	f.beads[beadID] = [2]string{status, assignee}
	return true, nil
}

func (f *fakeWorkReleaser) ResetSlot(agentID string) error {
	f.resets = append(f.resets, agentID)
	return f.resetErr
}

func (f *fakeWorkReleaser) Annotate(beadID, text string) error {
	if f.annotated == nil {
		f.annotated = map[string]string{}
	}
	f.annotated[beadID] = text
	return nil
}

// --- releasePolecatWork: the shared compare-and-release helper --------------

func TestReleasePolecatWorkComparesAssigneeBeforeRelease(t *testing.T) {
	t.Parallel()
	const me = "gastown/polecats/basalt"
	cases := []struct {
		name         string
		status, who  string
		readErr      error
		raceTo       string
		wantReleased bool
	}{
		{name: "hooked to this polecat", status: "hooked", who: me, wantReleased: true},
		// The common nuke case: the polecat was working. bd fences a plain
		// write here; the guarded write is the sanctioned transfer.
		{name: "in_progress on this polecat", status: "in_progress", who: me, wantReleased: true},
		{name: "re-slung to another polecat", status: "hooked", who: "gastown/polecats/granite"},
		{name: "re-slung between read and write", status: "in_progress", who: me, raceTo: "gastown/polecats/granite"},
		{name: "closed", status: "closed", who: me},
		{name: "already open", status: "open", who: ""},
		{name: "unreadable", readErr: errors.New("dolt down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {tc.status, tc.who}}, readErr: tc.readErr, raceTo: tc.raceTo}
			out := releasePolecatWork(rel, me, "gt-elvf4", false)
			if out.Released != tc.wantReleased || (len(rel.released) == 1) != tc.wantReleased {
				t.Fatalf("released = %v (%v), want %v; note %q", out.Released, rel.released, tc.wantReleased, out.SkipNote)
			}
			if !tc.wantReleased && out.SkipNote == "" {
				t.Fatal("a skipped release must say why")
			}
			if strings.Contains(out.SkipNote, "release failed") {
				t.Fatalf("a lost race must read as a skip, not a failure: %q", out.SkipNote)
			}
			if tc.raceTo != "" && rel.beads["gt-elvf4"][1] != tc.raceTo {
				t.Fatalf("the winner's assignment was overwritten: %v", rel.beads["gt-elvf4"])
			}
			if len(rel.resets) != 0 {
				t.Fatalf("resetSlot=false must not reset the slot, got %v", rel.resets)
			}
		})
	}
}

func TestReleasePolecatWorkResetsSlotOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	rel := &fakeWorkReleaser{beads: map[string][2]string{}}
	out := releasePolecatWork(rel, "gastown/polecats/basalt", "", true)
	if !out.SlotReset || len(rel.resets) != 1 || rel.resets[0] != "gastown/polecats/basalt" {
		t.Fatalf("slot reset = %v %v", out.SlotReset, rel.resets)
	}
	if len(rel.released) != 0 {
		t.Fatalf("no bead named, nothing to release: %v", rel.released)
	}
}

// A slot reset that fails must not be reported as done: SlotReset is the
// caller's signal that the polecat's agent bead really went idle (gt-34z9v).
func TestReleasePolecatWorkReportsSlotResetOnlyOnSuccess(t *testing.T) {
	t.Parallel()
	rel := &fakeWorkReleaser{beads: map[string][2]string{}, resetErr: errors.New("dolt down")}
	out := releasePolecatWork(rel, "gastown/polecats/basalt", "", true)
	if out.SlotReset {
		t.Fatal("a failed reset must not report the slot reset")
	}
}

// --- gt polecat nuke (gt-vm5g4) ---------------------------------------------

func survivesWith(branch string, err error) func(string) (string, error) {
	return func(string) (string, error) { return branch, err }
}

// survivesSeq answers the survival question with successive verdicts, one per
// call (start, removal, finish); the last one repeats.
func survivesSeq(verdicts ...[2]any) func(string) (string, error) {
	i := 0
	return func(string) (string, error) {
		v := verdicts[min(i, len(verdicts)-1)]
		i++
		branch, _ := v[0].(string)
		err, _ := v[1].(error)
		return branch, err
	}
}

func TestNukeHookedWorkEndToEnd(t *testing.T) {
	t.Parallel()
	const me = "gastown/polecats/basalt"
	const branch = "polecat/basalt/gt-elvf4+mu5wzd6q"
	working := &polecat.Polecat{Name: "basalt", Rig: "gastown", Issue: "gt-elvf4"}
	unreachable := errors.New("origin unreachable")

	for _, tc := range []struct {
		name         string
		status, who  string
		p            *polecat.Polecat
		readHook     func() string
		survives     func(string) (string, error)
		wantHeld     bool   // still hooked to the nuked polecat at the end
		wantComment  string // substring of the one comment ("" = no comment)
		wantAttempts int    // release writes attempted
		wantAsks     int    // survival questions put to the predicate
	}{
		{name: "hooked, surviving work: still hooked after removal", status: "hooked", who: me, p: working,
			survives: survivesWith(branch, nil), wantHeld: true, wantComment: "gt sling gt-elvf4 gastown --branch " + branch, wantAsks: 2},
		{name: "in_progress, surviving work: kept, one comment, no release attempts", status: "in_progress", who: me, p: working,
			survives: survivesWith(branch, nil), wantHeld: true, wantComment: "--branch " + branch, wantAsks: 2},
		{name: "hooked, merged branch: released", status: "hooked", who: me, p: working,
			survives: survivesWith("", nil), wantAttempts: 1, wantAsks: 1},
		{name: "survival unknown: kept, unknown comment", status: "in_progress", who: me, p: working,
			survives: survivesWith("", unreachable), wantHeld: true, wantAsks: 2,
			wantComment: "hook kept: could not verify surviving work; run gt polecat surviving-work gt-elvf4"},
		{name: "rig with no git repo: released", status: "hooked", who: me, p: working,
			survives: survivesWith("", polecat.ErrNoRigRepo), wantAttempts: 1, wantAsks: 1},
		{name: "survived before removal, gone after the branch delete: released at finish", status: "hooked", who: me, p: working,
			survives: survivesSeq([2]any{branch}, [2]any{""}), wantAttempts: 1, wantAsks: 2},
		{name: "survived before removal, unknown after: kept, unknown comment", status: "hooked", who: me, p: working,
			survives: survivesSeq([2]any{branch}, [2]any{"", unreachable}), wantHeld: true, wantAsks: 2,
			wantComment: "could not verify surviving work"},
		{name: "unknown before removal, survives after: comment names the final branch", status: "hooked", who: me, p: working,
			survives: survivesSeq([2]any{"", unreachable}, [2]any{branch}), wantHeld: true, wantAsks: 2,
			wantComment: "--branch " + branch},
		{name: "reaped before the nuke: hook read off the agent bead, released", status: "hooked", who: me,
			readHook: func() string { return "gt-elvf4" }, survives: survivesWith("", nil), wantAttempts: 1, wantAsks: 1},
		{name: "reaped before the nuke with surviving work: kept", status: "hooked", who: me,
			readHook: func() string { return "gt-elvf4" }, survives: survivesWith(branch, nil), wantHeld: true, wantAsks: 2, wantComment: "--branch " + branch},
		{name: "already re-slung elsewhere: untouched", status: "hooked", who: "gastown/polecats/granite", p: working,
			survives: survivesWith(branch, nil), wantAsks: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := &countingReleaser{fakeWorkReleaser: fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {tc.status, tc.who}}}}
			asks := runNukeHookFlowCounting(rel, tc.p, tc.readHook, tc.survives)

			cur := rel.beads["gt-elvf4"]
			if held := cur[1] == me; held != tc.wantHeld {
				t.Fatalf("held by nuked polecat = %v (%v), want %v", held, cur, tc.wantHeld)
			}
			if tc.who != me && cur[1] != tc.who {
				t.Fatalf("another agent's bead changed: %v", cur)
			}
			note, commented := rel.annotated["gt-elvf4"]
			if commented != (tc.wantComment != "") || !strings.Contains(note, tc.wantComment) {
				t.Fatalf("comment = %q (present %v), want %q", note, commented, tc.wantComment)
			}
			if rel.attempts != tc.wantAttempts {
				t.Fatalf("release attempts = %d, want %d", rel.attempts, tc.wantAttempts)
			}
			if asks != tc.wantAsks {
				t.Fatalf("survival questions asked = %d, want %d (the hooked bead is judged once before removal and re-checked once at the end)", asks, tc.wantAsks)
			}
		})
	}
}

// countingReleaser counts release writes, so "kept" also means "never tried".
type countingReleaser struct {
	fakeWorkReleaser
	attempts int
}

func (c *countingReleaser) ReleaseBead(beadID, expected string) (bool, error) {
	c.attempts++
	return c.fakeWorkReleaser.ReleaseBead(beadID, expected)
}

// The verdict the nuke reaches before removal is the one it hands to removal,
// so the two steps cannot ask the same bead separately and act on different
// answers (gt-kud90).
func TestNukeHandsItsVerdictToRemoval(t *testing.T) {
	t.Parallel()
	const me = "gastown/polecats/basalt"
	const branch = "polecat/basalt/gt-elvf4+mu5wzd6q"
	working := &polecat.Polecat{Name: "basalt", Rig: "gastown", Issue: "gt-elvf4"}

	for _, tc := range []struct {
		name         string
		survives     func(string) (string, error)
		wantSurvives string
		wantUnknown  bool
		wantNothing  bool // released at the start: there is no hook left to hand on
	}{
		{name: "surviving work hands the branch on", survives: survivesWith(branch, nil), wantSurvives: branch},
		{name: "an unknown answer hands the unknown on", survives: survivesWith("", errors.New("origin unreachable")), wantUnknown: true},
		{name: "nothing to protect releases the bead instead", survives: survivesWith("", nil), wantNothing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-elvf4": {"in_progress", me}}}
			h := startNukeHookedWork(rel, tc.survives, "gastown", "basalt", working, nil)

			judged := h.judged()
			if tc.wantNothing {
				if judged != nil {
					t.Fatalf("judged = %v, want none: the hook was released", judged)
				}
				return
			}
			v, ok := judged["gt-elvf4"]
			if !ok || v.SurvivesOn() != tc.wantSurvives || v.Unknown() != tc.wantUnknown {
				t.Fatalf("judged = %+v (present %v), want survives=%q unknown=%v", v, ok, tc.wantSurvives, tc.wantUnknown)
			}
		})
	}
}

// runNukeHookFlowCounting drives the nuke's hooked-work steps in order: decide
// before removal, remove (mgr.RemoveWithOptions' unassignWorkBeads replays the
// verdict the start reached for the hooked bead instead of asking again), then
// report after. It returns how many questions the flow put to the predicate,
// so a bead judged twice is visible.
func runNukeHookFlowCounting(rel *countingReleaser, p *polecat.Polecat, readHook func() string, survives func(string) (string, error)) (asks int) {
	ask := func(beadID string) (string, error) {
		asks++
		return survives(beadID)
	}
	h := startNukeHookedWork(rel, ask, "gastown", "basalt", p, readHook)
	if verdict, ok := h.judged()["gt-elvf4"]; ok && verdict.NothingToProtect() {
		// Removal's own guarded release on the replayed verdict: a no-op when
		// the start already released the bead.
		if held, _ := heldBy(rel, "gastown/polecats/basalt", "gt-elvf4"); held {
			_, _ = rel.ReleaseBead("gt-elvf4", "gastown/polecats/basalt")
		}
	}
	h.finish()
	return asks
}

// submittedReleaser is a fakeWorkReleaser that also answers whether a bead
// is submitted for landing.
type submittedReleaser struct {
	*fakeWorkReleaser
	submitted map[string]bool
	err       error
}

// TestRestoreOriginalHoldReportsAFailedRestore covers gt-34z9v: a restore that
// fails is returned to the caller, which then still releases the bead, instead
// of the bead being dropped from release while it stays hooked to the polecat
// being removed. The failure is also left on the bead as a comment.
func TestRestoreOriginalHoldReportsAFailedRestore(t *testing.T) {
	t.Parallel()
	const me = "gastown/polecats/basalt"
	for _, tc := range []struct {
		name        string
		restoreErr  error
		wantHandled bool
		wantErr     bool
		wantComment bool
	}{
		{name: "restore succeeds", wantHandled: true},
		{name: "restore fails: reported and annotated", restoreErr: errors.New("dolt down"), wantErr: true, wantComment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", me}}, restoreErr: tc.restoreErr}
			surviving := func(string, string) (string, error) { return "polecat/basalt/gt-abc+mu5wzd6q", nil }

			handled, err := restoreOriginalHoldIfWorkSurvivesWith(rel, surviving, "/town", me, "gt-abc", &beadHold{Status: "hooked", Assignee: me})

			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tc.wantHandled)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error = %v", err, tc.wantErr)
			}
			if commented := rel.annotated["gt-abc"] != ""; commented != tc.wantComment {
				t.Fatalf("comment = %q (present %v), want comment present = %v", rel.annotated["gt-abc"], commented, tc.wantComment)
			}
		})
	}
}

func (s submittedReleaser) SubmittedForLanding(beadID string) (bool, error) {
	return s.submitted[beadID], s.err
}

func TestReleasePolecatWorkLeavesSubmittedWork(t *testing.T) {
	t.Parallel()
	const me = "gastown/polecats/basalt"
	for _, tc := range []struct {
		name      string
		submitted bool
		err       error
		want      bool
	}{
		{name: "submitted for landing", submitted: true},
		{name: "unreadable label", err: errors.New("dolt down")},
		{name: "not submitted", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := submittedReleaser{fakeWorkReleaser: &fakeWorkReleaser{beads: map[string][2]string{"gt-x": {"hooked", me}}},
				submitted: map[string]bool{"gt-x": tc.submitted}, err: tc.err}
			out := releasePolecatWork(rel, me, "gt-x", false)
			if out.Released != tc.want {
				t.Fatalf("released = %v, want %v (note %q)", out.Released, tc.want, out.SkipNote)
			}
			if !tc.want && out.SkipNote == "" {
				t.Fatal("a skipped release must say why")
			}
		})
	}
}
