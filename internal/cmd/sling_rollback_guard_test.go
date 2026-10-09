package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// --- fakes ------------------------------------------------------------------

type fakeSandbox struct {
	removed  []string
	branches []string
}

func (f *fakeSandbox) RemovePolecat(name string) error {
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeSandbox) DeleteBranch(branch string) { f.branches = append(f.branches, branch) }

// --- cleanupSpawnedPolecatWork: undo only what this sling created -----------

func TestCleanupSpawnedPolecatWorkRespectsProvenance(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/Toast"
	cases := []struct {
		name         string
		fresh        bool
		created      bool
		wantRemoved  bool
		wantBranchRm bool
		wantReset    bool
	}{
		{name: "fresh spawn, created branch", fresh: true, created: true, wantRemoved: true, wantBranchRm: true},
		{name: "fresh spawn, resumed branch", fresh: true, created: false, wantRemoved: true},
		{name: "reused sandbox, created branch", fresh: false, created: true, wantReset: true},
		{name: "reused sandbox, resumed branch", fresh: false, created: false, wantReset: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", agent}}}
			f := newRollbackFixture(t, nil, rel)
			sb := f.sb

			f.r.cleanupSpawned(&SpawnedPolecatInfo{
				RigName: "gastown", PolecatName: "Toast", Branch: "polecat/Toast/gt-abc",
				FreshSpawn: tc.fresh, BranchCreated: tc.created,
			}, "gastown", "gt-abc", "")

			if got := len(sb.removed) == 1; got != tc.wantRemoved {
				t.Errorf("sandbox removed = %v, want %v", sb.removed, tc.wantRemoved)
			}
			if got := len(sb.branches) == 1; got != tc.wantBranchRm {
				t.Errorf("branch delete = %v, want %v", sb.branches, tc.wantBranchRm)
			}
			if got := len(rel.resets) == 1; got != tc.wantReset {
				t.Errorf("slot reset = %v, want %v", rel.resets, tc.wantReset)
			}
			if len(rel.released) != 1 {
				t.Errorf("the hook this sling set must be released, got %v", rel.released)
			}
		})
	}
}

// A failed sling whose bead carries surviving work (e.g. a --branch resume)
// hands the bead back to its pre-sling holder instead of releasing it to a
// fresh re-dispatch from main; an unknown answer does the same.
func TestCleanupSpawnedPolecatWorkRestoresOriginalHoldWhenWorkSurvives(t *testing.T) {
	t.Parallel()
	const toast = "gastown/polecats/Toast"
	const pearl = "gastown/polecats/pearl"
	for _, tc := range []struct {
		name        string
		branch      string
		err         error
		orig        *beadHold
		wantHolder  string
		wantStatus  string
		wantRelease bool
	}{
		{name: "work survives: back to the original holder", branch: "polecat/pearl/gt-abc+mu5wzd6q",
			orig: &beadHold{Status: "hooked", Assignee: pearl}, wantHolder: pearl, wantStatus: "hooked"},
		{name: "survival unknown: back to the original holder", err: errors.New("origin unreachable"),
			orig: &beadHold{Status: "in_progress", Assignee: pearl}, wantHolder: pearl, wantStatus: "in_progress"},
		{name: "no surviving work: released", orig: &beadHold{Status: "hooked", Assignee: pearl},
			wantStatus: "open", wantRelease: true},
		{name: "original unknown: released", branch: "polecat/pearl/gt-abc+mu5wzd6q",
			wantStatus: "open", wantRelease: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", toast}}}
			f := newRollbackFixture(t, nil, rel)
			f.r.survivingWork = func(string, string) (string, error) { return tc.branch, tc.err }

			info := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", FreshSpawn: true, originalHold: tc.orig}
			f.r.cleanupSpawned(info, "gastown", "gt-abc", "")

			got := rel.beads["gt-abc"]
			if got[0] != tc.wantStatus || got[1] != tc.wantHolder {
				t.Fatalf("bead = %v, want status %q holder %q", got, tc.wantStatus, tc.wantHolder)
			}
			if (len(rel.released) == 1) != tc.wantRelease {
				t.Fatalf("released = %v, want release %v", rel.released, tc.wantRelease)
			}
		})
	}
}

func TestCleanupSpawnedPolecatWorkNeverUnhooksAnotherAssignee(t *testing.T) {
	t.Parallel()
	rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", "gastown/polecats/granite"}}}
	f := newRollbackFixture(t, nil, rel)

	f.r.cleanupSpawned(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", FreshSpawn: true},
		"gastown", "gt-abc", "")
	if len(rel.released) != 0 {
		t.Fatalf("released a bead hooked to someone else: %v", rel.released)
	}
}

// A restore that fails must not drop the bead from release: the bead is still
// hooked to the polecat being removed, so it is released instead of left
// wedged there (gt-34z9v).
func TestCleanupSpawnedPolecatWorkReleasesWhenRestoreFails(t *testing.T) {
	t.Parallel()
	const toast = "gastown/polecats/Toast"
	rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", toast}}, restoreErr: errors.New("dolt down")}
	f := newRollbackFixture(t, nil, rel)
	f.r.survivingWork = func(string, string) (string, error) { return "polecat/pearl/gt-abc+mu5wzd6q", nil }

	f.r.cleanupSpawned(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", FreshSpawn: true,
		originalHold: &beadHold{Status: "hooked", Assignee: "gastown/polecats/pearl"}}, "gastown", "gt-abc", "")

	if got := rel.beads["gt-abc"]; got[0] != "open" || got[1] != "" {
		t.Fatalf("bead = %v, want open with no assignee (released, not left hooked)", got)
	}
	if len(rel.released) != 1 {
		t.Fatalf("released = %v, want one release", rel.released)
	}
	if rel.annotated["gt-abc"] == "" {
		t.Fatal("the failed restore must be left on the bead as a comment")
	}
}

// The zero value is the safe one: an info built without provenance keeps the
// sandbox and the branch.
func TestCleanupSpawnedPolecatWorkZeroProvenanceKeepsEverything(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, nil, nil)
	sb := f.sb

	f.r.cleanupSpawned(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", Branch: "feature/x"}, "gastown", "", "")
	if len(sb.removed) != 0 || len(sb.branches) != 0 {
		t.Fatalf("zero-provenance cleanup destroyed %v / %v", sb.removed, sb.branches)
	}
}

// --- runFormula: one deferred rollback guard (gt-7evi4) ---------------------

var errInjected = errors.New("injected failure")

// TestRunSlingFormulaRollsBackOnEveryPostSpawnExit: once target resolution
// has spawned a polecat, every exit short of the commit point rolls it back
// exactly once, naming the wisp only once the sling is about to hook it, and
// burns every wisp the sling created and did not commit.
func TestRunSlingFormulaRollsBackOnEveryPostSpawnExit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		inject       func(h *slingHarness)
		wantErrSub   string // "" = the sling succeeds
		wantRollback bool
		wantBeadID   string
		wantBurned   []string // wisps the sling burned
		target       string   // default "gastown" (rig target)
	}{
		{name: "hooked-formula lookup fails", wantErrSub: "checking existing hooked formulas", wantRollback: true, inject: func(h *slingHarness) {
			h.run.findHookedFormula = func(string, string, string) (*beads.Issue, error) { return nil, errInjected }
		}},
		// A wisp still hooked to a just-spawned polecat's identity is stale:
		// it is burned and the sling dispatches fresh, instead of a "no-op"
		// that leaves it hooked to a polecat nobody starts.
		{name: "stale formula wisp is burned, then dispatch succeeds", wantBurned: []string{"gt-wisp-existing"}, inject: func(h *slingHarness) {
			h.hookedFormulas["gastown/polecats/Toast"] = &beads.Issue{ID: "gt-wisp-existing"}
		}},
		{name: "stale formula wisp burn fails", wantErrSub: "burning stale formula wisp", wantRollback: true, inject: func(h *slingHarness) {
			h.hookedFormulas["gastown/polecats/Toast"] = &beads.Issue{ID: "gt-wisp-existing"}
			h.run.burnWisp = func(string, string) error { return errInjected }
		}},
		// A named polecat with no session is spawned by name; its admission
		// is taken after the spawn, so a refusal there rolls the spawn back.
		{name: "formula admission fails", target: "gastown/polecats/toast", wantErrSub: "injected failure", wantRollback: true, inject: func(h *slingHarness) {
			h.run.admitPolecat = func(string, string, string, string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error) {
				return nil, polecatCapacitySnapshot{}, errInjected
			}
		}},
		{name: "cook fails", wantErrSub: "cooking formula", wantRollback: true, inject: func(h *slingHarness) {
			h.run.cookFormula = func(string, string, string) error { return errInjected }
		}},
		{name: "wisp create fails", wantErrSub: "creating wisp", wantRollback: true, inject: func(h *slingHarness) {
			h.run.createWisp = func(string, string, string, []string) ([]byte, error) { return nil, errInjected }
		}},
		{name: "wisp output unparseable", wantErrSub: "parsing wisp", wantRollback: true, inject: func(h *slingHarness) {
			h.run.createWisp = func(string, string, string, []string) ([]byte, error) { return []byte("not json"), nil }
		}},
		{name: "hook fails", wantErrSub: "injected failure", wantRollback: true, wantBeadID: "gt-wisp-new", wantBurned: []string{"gt-wisp-new"}, inject: func(h *slingHarness) {
			h.run.hookWisp = func(string, string, string) error { return errInjected }
		}},
		{name: "session start fails", wantErrSub: "starting polecat session", wantRollback: true, wantBeadID: "gt-wisp-new", wantBurned: []string{"gt-wisp-new"}, inject: func(h *slingHarness) {
			h.run.startSession = func(*SpawnedPolecatInfo) (string, error) { return "", errInjected }
		}},
		{name: "success commits", inject: func(*slingHarness) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.run.resolveTarget = h.run.resolveSlingTarget
			tc.inject(h)

			target := tc.target
			if target == "" {
				target = "gastown"
			}
			err := h.run.runFormula(context.Background(), []string{"mol-test", target})
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("runFormula: %v", err)
				}
			} else {
				wantSlingErr(t, err, tc.wantErrSub)
			}
			var wantRollback []string
			if tc.wantRollback {
				wantRollback = []string{"rollback Toast bead=" + tc.wantBeadID}
			}
			h.wantCalls("rollback", wantRollback...)
			var wantBurned []string
			for _, id := range tc.wantBurned {
				wantBurned = append(wantBurned, "burn wisp "+id)
			}
			h.wantCalls("burn wisp", wantBurned...)
		})
	}
}
