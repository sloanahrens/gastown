package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// The nuke's hooked-work ordering — decide before the sandbox is removed, then
// re-check after — is driven here through nukePolecatFullWithOptions itself,
// not through a helper that restates it: a real polecat Manager, its removal,
// and the verdict it hands on.
//
// What is injected, and why:
//   - the bead store, because a unit tier starts no bd (docs/testing.md,
//     "The rules": the refusing stand-ins in internal/testutil/unittier);
//   - the session stopper, for the same reason, one step earlier;
//   - the survival answer, because the unit tier runs no real git from a test
//     file at all (internal/testpolicy's TestRealGit; realgit.txt is empty
//     tree-wide), and the production predicate opens git itself. The predicate
//     is tested where it lives (internal/polecat), and what this file pins is
//     the ordering and the handoff: the verdict reached before removal is the
//     one removal replays, and the comment is written only after it.
//
// The sibling unit test (polecat_work_release_test.go) calls
// startNukeHookedWork and finish by hand, so it pins the steps but not their
// place in the nuke; this one pins both.

const (
	nukeE2ERig     = "gastown"
	nukeE2EPolecat = "basalt"
	nukeE2EAgent   = "gastown/polecats/basalt"
	nukeE2EBead    = "gt-bxrji"
	nukeE2EBranch  = "polecat/basalt/gt-bxrji+m1abcd"
)

// releaserCall is one entry in the stub's log: what the nuke asked for, and
// whether the polecat sandbox was already gone when it asked. The second half
// is what makes the order observable: the release lands before removal, the
// survival re-check and its comment after it.
type releaserCall struct {
	op string
	// removed reports that the sandbox no longer existed at call time.
	removed bool
}

// recordingReleaser is the bd stub. It answers the polecatWorkReleaser surface
// in memory and writes every call to an ordered log, so an assertion reads the
// order production actually used instead of restating it.
type recordingReleaser struct {
	*fakeWorkReleaser
	// sandboxGone reports whether the polecat's sandbox is gone yet.
	sandboxGone func() bool
	calls       []releaserCall
}

func (r *recordingReleaser) record(op string) {
	r.calls = append(r.calls, releaserCall{op: op, removed: r.sandboxGone()})
}

func (r *recordingReleaser) HookState(beadID string) (string, string, error) {
	r.record("keep-check")
	return r.fakeWorkReleaser.HookState(beadID)
}

func (r *recordingReleaser) ReleaseBead(beadID, expectedAssignee string) (bool, error) {
	r.record("release")
	return r.fakeWorkReleaser.ReleaseBead(beadID, expectedAssignee)
}

func (r *recordingReleaser) Annotate(beadID, text string) error {
	r.record("annotate")
	return r.fakeWorkReleaser.Annotate(beadID, text)
}

// stubStore is the Manager's bead database in memory: beadsfake answers the
// Client surface, and the two polecat-lifecycle helpers bd's store adds are
// recorded. It is what keeps a real Manager from shelling out to bd.
type stubStore struct {
	*beadsfake.Fake
	reassignments []string
}

func (s *stubStore) RecordReassignment(id, from, to, requester string, branches []string) error {
	s.reassignments = append(s.reassignments, fmt.Sprintf("%s %s->%s", id, from, to))
	return nil
}

func (s *stubStore) FindMRForBranchAny(string) (*beads.Issue, error) { return nil, nil }

// noSession is the session stopper for a seat that has no session: nuke's
// first step treats ErrSessionNotFound as "nothing to kill" and prints nothing.
type noSession struct{ stops int }

func (n *noSession) Stop(string, bool) error {
	n.stops++
	return polecat.ErrSessionNotFound
}

// nukeE2E is one temp town: a rig with a polecat seat holding one hooked bead.
// The seat is a plain directory — the nuke only has to remove it, and the test
// file may not create a real git repository to put there.
type nukeE2E struct {
	t         *testing.T
	rigPath   string
	polecatAt string
	store     *stubStore
}

func newNukeE2E(t *testing.T) *nukeE2E {
	t.Helper()
	rigPath := filepath.Join(t.TempDir(), nukeE2ERig)
	f := &nukeE2E{
		t:         t,
		rigPath:   rigPath,
		polecatAt: filepath.Join(rigPath, "polecats", nukeE2EPolecat, nukeE2ERig),
	}
	if err := os.MkdirAll(f.polecatAt, 0o755); err != nil {
		t.Fatalf("creating the polecat seat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.polecatAt, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("writing into the polecat seat: %v", err)
	}

	// The work bead the polecat holds, as the Manager's store sees it: hooked
	// to this polecat, which is what makes the nuke judge it at all.
	f.store = &stubStore{Fake: beadsfake.New()}
	issue, err := f.store.Create(beads.CreateOptions{ID: nukeE2EBead, Title: "hooked work", Assignee: nukeE2EAgent})
	if err != nil {
		t.Fatalf("creating the work bead: %v", err)
	}
	status := string(beads.StatusHooked)
	if err := f.store.Update(issue.ID, beads.UpdateOptions{Status: &status}); err != nil {
		t.Fatalf("hooking the work bead: %v", err)
	}
	return f
}

// nuke drives the production nuke over this fixture with the survival question
// answered by the caller, and returns the stub whose log holds the order the
// nuke asked for things in.
func (f *nukeE2E) nuke(survives func(beadID string) (string, error), opts nukePolecatOptions) (*recordingReleaser, *noSession) {
	f.t.Helper()

	releaser := &recordingReleaser{
		fakeWorkReleaser: &fakeWorkReleaser{
			beads: map[string][2]string{nukeE2EBead: {"hooked", nukeE2EAgent}},
		},
		sandboxGone: func() bool {
			_, err := os.Stat(f.polecatAt)
			return err != nil
		},
	}
	sessions := &noSession{}
	opts.Deps = &nukeDeps{Sessions: sessions, Releaser: releaser, SurvivingWork: survives, MoleculeBeads: f.store}

	r := &rig.Rig{Name: nukeE2ERig, Path: f.rigPath}
	mgr := polecat.NewManagerWithStore(r, nil, nil, townRegistry(), f.store)
	if err := nukePolecatFullWithOptions(nukeE2EPolecat, nukeE2ERig, mgr, r, opts); err != nil {
		f.t.Fatalf("nuke returned %v, want nil", err)
	}
	if sessions.stops != 1 {
		f.t.Errorf("session stops = %d, want exactly 1 (the kill is the first step)", sessions.stops)
	}
	return releaser, sessions
}

func (f *nukeE2E) assertSandboxGone() {
	f.t.Helper()
	if _, err := os.Stat(f.polecatAt); err == nil {
		f.t.Errorf("polecat sandbox %s still exists after the nuke", f.polecatAt)
	}
}

// TestNukeKeepsHookedWorkWhenThePolecatBranchSurvives drives the nuke with
// unmerged work on a polecat branch: the bead stays hooked and gets the resume
// comment, so a re-sling cannot start a fresh polecat from main over it.
func TestNukeKeepsHookedWorkWhenThePolecatBranchSurvives(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	releaser, _ := f.nuke(func(string) (string, error) { return nukeE2EBranch, nil }, nukePolecatOptions{})

	// The order is read off the stub's log, not restated from the source: the
	// keep-check, then the re-check and the comment, with the second check
	// landing only once the sandbox is gone.
	want := []releaserCall{
		{op: "keep-check", removed: false},
		{op: "keep-check", removed: true},
		{op: "annotate", removed: true},
	}
	if got := fmt.Sprint(releaser.calls); got != fmt.Sprint(want) {
		t.Errorf("bd calls =\n  %v\nwant\n  %v", releaser.calls, want)
	}
	if len(releaser.released) != 0 {
		t.Errorf("released %v, want no release: the work survives", releaser.released)
	}
	if got := releaser.beads[nukeE2EBead]; got != [2]string{"hooked", nukeE2EAgent} {
		t.Errorf("bead after the nuke = %v, want it still hooked to %s", got, nukeE2EAgent)
	}
	note := releaser.annotated[nukeE2EBead]
	if !strings.Contains(note, "--branch "+nukeE2EBranch) || !strings.Contains(note, "gt sling "+nukeE2EBead) {
		t.Errorf("resume comment = %q, want it naming the branch and the resume command", note)
	}

	f.assertSandboxGone()
}

// TestNukeReleasesHookedWorkWhenTheWorkIsMerged is the other definite answer:
// nothing survives to protect, so the bead goes back to open before the
// sandbox is removed.
func TestNukeReleasesHookedWorkWhenTheWorkIsMerged(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	releaser, _ := f.nuke(func(string) (string, error) { return "", nil }, nukePolecatOptions{})

	want := []releaserCall{
		{op: "keep-check", removed: false},
		{op: "keep-check", removed: false},
		{op: "release", removed: false},
	}
	if got := fmt.Sprint(releaser.calls); got != fmt.Sprint(want) {
		t.Errorf("bd calls =\n  %v\nwant\n  %v", releaser.calls, want)
	}
	if len(releaser.released) != 1 || releaser.released[0] != nukeE2EBead {
		t.Errorf("released = %v, want the merged bead released once", releaser.released)
	}
	if len(releaser.annotated) != 0 {
		t.Errorf("annotated %v, want no comment on a released bead", releaser.annotated)
	}
	if got := releaser.beads[nukeE2EBead]; got != [2]string{"open", ""} {
		t.Errorf("bead after the nuke = %v, want it back to open and unassigned", got)
	}
	// Removal records the assignment it is about to clear, so the bead keeps
	// the name of the polecat whose branch it came from (gt-zd7c).
	wantReassignment := nukeE2EBead + " " + nukeE2EAgent + "->"
	if got := fmt.Sprint(f.store.reassignments); got != fmt.Sprint([]string{wantReassignment}) {
		t.Errorf("recorded reassignments = %v, want [%q]", f.store.reassignments, wantReassignment)
	}

	f.assertSandboxGone()
}

// TestNukeKeepsHookedWorkWhenSurvivalIsUnknown pins the fail-closed answer:
// when the survival question cannot be answered, the bead stays hooked with a
// comment saying so, because releasing it would hand possibly-preserved work
// to a fresh polecat.
func TestNukeKeepsHookedWorkWhenSurvivalIsUnknown(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	releaser, _ := f.nuke(func(string) (string, error) {
		return "", errors.New("could not read origin")
	}, nukePolecatOptions{})

	want := []releaserCall{
		{op: "keep-check", removed: false},
		{op: "keep-check", removed: true},
		{op: "annotate", removed: true},
	}
	if got := fmt.Sprint(releaser.calls); got != fmt.Sprint(want) {
		t.Errorf("bd calls =\n  %v\nwant\n  %v", releaser.calls, want)
	}
	if len(releaser.released) != 0 {
		t.Errorf("released %v, want no release: an unknown answer must not release", releaser.released)
	}
	if got := releaser.beads[nukeE2EBead]; got != [2]string{"hooked", nukeE2EAgent} {
		t.Errorf("bead after the nuke = %v, want it still hooked to %s", got, nukeE2EAgent)
	}
	note := releaser.annotated[nukeE2EBead]
	if !strings.Contains(note, "could not verify surviving work") {
		t.Errorf("comment = %q, want it saying survival could not be verified", note)
	}

	f.assertSandboxGone()
}

// TestNukeHandsThePreRemovalVerdictToRemoval is the reason the three tests
// above can rely on the answer at all: removal re-asks the survival question
// unless it is handed the verdict the nuke already reached, and the fixture's
// rig has no repo to answer it from. Here the injected predicate must be asked
// exactly twice (decide, then re-check) — a third ask would mean removal went
// looking on its own.
func TestNukeHandsThePreRemovalVerdictToRemoval(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	asked := 0
	releaser, _ := f.nuke(func(string) (string, error) {
		asked++
		return nukeE2EBranch, nil
	}, nukePolecatOptions{})

	if asked != 2 {
		t.Errorf("survival questions = %d, want 2 (the nuke's own decide, then its re-check): removal must replay the verdict it was handed", asked)
	}
	if len(releaser.calls) == 0 || releaser.calls[len(releaser.calls)-1].op != "annotate" {
		t.Errorf("calls = %v, want the comment last", releaser.calls)
	}
}
