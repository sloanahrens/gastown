package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
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

// installRollbackFakes swaps the bead and rig seams the rollback touches for
// fakes, so no test here reaches bd, git or a polecat manager.
func installRollbackFakes(t *testing.T, rel *fakeWorkReleaser) *fakeSandbox {
	t.Helper()
	sb := &fakeSandbox{}
	prevRel, prevSandbox, prevSurviving := newPolecatWorkReleaserFn, openSpawnedPolecatSandboxFn, survivingWorkForBeadFn
	newPolecatWorkReleaserFn = func(string, string) polecatWorkReleaser { return rel }
	openSpawnedPolecatSandboxFn = func(string, string) (spawnedPolecatSandbox, error) { return sb, nil }
	survivingWorkForBeadFn = func(string, string) (string, error) { return "", nil }
	t.Cleanup(func() {
		newPolecatWorkReleaserFn, openSpawnedPolecatSandboxFn, survivingWorkForBeadFn = prevRel, prevSandbox, prevSurviving
	})
	return sb
}

// --- cleanupSpawnedPolecatWork: undo only what this sling created -----------

func chdirTempTown(t *testing.T) string {
	t.Helper()
	townRoot, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

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
			}, "gastown", "gt-abc", "", "")

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
			f.r.cleanupSpawned(info, "gastown", "gt-abc", "", "")

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
		"gastown", "gt-abc", "", "")
	if len(rel.released) != 0 {
		t.Fatalf("released a bead hooked to someone else: %v", rel.released)
	}
}

// The zero value is the safe one: an info built without provenance keeps the
// sandbox and the branch.
func TestCleanupSpawnedPolecatWorkZeroProvenanceKeepsEverything(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, nil, nil)
	sb := f.sb

	f.r.cleanupSpawned(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", Branch: "feature/x"}, "gastown", "", "", "")
	if len(sb.removed) != 0 || len(sb.branches) != 0 {
		t.Fatalf("zero-provenance cleanup destroyed %v / %v", sb.removed, sb.branches)
	}
}

// --- runSling: one deferred rollback guard (gt-7evi4) -----------------------

const rollbackGuardBDStub = `#!/bin/sh
cmd="$1"
shift || true
while [ "$cmd" = "--db" ] || [ "$cmd" = "--allow-stale" ]; do
  if [ "$cmd" = "--db" ]; then
    shift || true
  fi
  cmd="$1"
  shift || true
done
if [ -n "$BD_FAIL" ] && [ "$cmd" = "$BD_FAIL" ]; then
  echo "forced $cmd failure" 1>&2
  exit 1
fi
case "$cmd" in
  show)
    echo "[{\"title\":\"Test issue\",\"status\":\"${BD_STATUS:-open}\",\"assignee\":\"\",\"description\":\"\"}]"
    ;;
  mol)
    if [ "$1" = "wisp" ]; then
      out="$BD_WISP_OUT"
      [ -z "$out" ] && out='{"root_id":"gt-wisp-new"}'
      echo "$out"
    fi
    ;;
esac
exit 0
`

// setupRollbackGuardTown builds a temp town with rig gastown and a bd stub, and
// resets every sling flag and seam the guard tests touch.
func setupRollbackGuardTown(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	townRoot := chdirTempTown(t)
	rigs := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{
		"gastown": {GitURL: "git@github.com:test/gastown.git", AddedAt: time.Now().Truncate(time.Second),
			BeadsConfig: &config.BeadsConfig{Repo: "local", Prefix: "gt-"}},
	}}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), rigs); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join("gastown", "mayor", "rig", ".beads"), ".beads", "bin"} {
		if err := os.MkdirAll(filepath.Join(townRoot, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" + `{"prefix":"hq-","path":"."}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(townRoot, "bin")
	_ = writeBDStub(t, binDir, rollbackGuardBDStub, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")
	t.Setenv("BD_FAIL", "")
	t.Setenv("BD_STATUS", "")
	t.Setenv("BD_WISP_OUT", "")

	prev := struct {
		noConvoy, noBoot, dryRun, hookRaw, noMerge, reviewOnly, force, ralph bool
		formula, resume, base, onTarget                                      string
		vars                                                                 []string
		spawn                                                                func(string, SlingSpawnOptions) (*SpawnedPolecatInfo, error)
		rollback                                                             func(*SpawnedPolecatInfo, string, string, string)
		storeRaw                                                             func(string, string, beadFieldUpdates) error
		session                                                              func(*SpawnedPolecatInfo) (string, error)
		hook                                                                 func(string, string, string) error
		findFormula                                                          func(string, string, string) (*beads.Issue, error)
		admission                                                            func(string, string, string, string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error)
	}{slingNoConvoy, slingNoBoot, slingDryRun, slingHookRawBead, slingNoMerge, slingReviewOnly, slingForce, slingRalph,
		slingFormula, slingResumeBranch, slingBaseBranch, slingOnTarget, slingVars,
		spawnPolecatForSling, rollbackSlingArtifactsFn, storeRawSlingMetadataFn, startSpawnedPolecatSessionFn,
		hookBeadWithRetryFn, findHookedFormulaSingletonFn, acquirePolecatAdmissionFn}
	t.Cleanup(func() {
		slingNoConvoy, slingNoBoot, slingDryRun, slingHookRawBead = prev.noConvoy, prev.noBoot, prev.dryRun, prev.hookRaw
		slingNoMerge, slingReviewOnly, slingForce, slingRalph = prev.noMerge, prev.reviewOnly, prev.force, prev.ralph
		slingFormula, slingResumeBranch, slingBaseBranch, slingOnTarget, slingVars = prev.formula, prev.resume, prev.base, prev.onTarget, prev.vars
		spawnPolecatForSling, rollbackSlingArtifactsFn = prev.spawn, prev.rollback
		storeRawSlingMetadataFn, startSpawnedPolecatSessionFn = prev.storeRaw, prev.session
		hookBeadWithRetryFn, findHookedFormulaSingletonFn, acquirePolecatAdmissionFn = prev.hook, prev.findFormula, prev.admission
	})

	prevBurnWisp, prevResolveAgent := burnSlingWispFn, resolveTargetAgentFn
	prevClearLabels := clearOrphanEpisodeLabelsFn
	t.Cleanup(func() {
		burnSlingWispFn, resolveTargetAgentFn = prevBurnWisp, prevResolveAgent
		clearOrphanEpisodeLabelsFn = prevClearLabels
	})
	clearOrphanEpisodeLabelsFn = func(string, string, string) {}
	burnSlingWispFn = func(string, string) error { return nil }

	slingNoConvoy, slingNoBoot, slingDryRun, slingHookRawBead = true, true, false, false
	slingNoMerge, slingReviewOnly, slingForce, slingRalph = false, false, false, false
	slingFormula, slingResumeBranch, slingBaseBranch, slingOnTarget, slingVars = "", "", "", "", nil

	fakeWorkDir := filepath.Join(townRoot, "fake-polecat")
	if err := os.MkdirAll(fakeWorkDir, 0755); err != nil {
		t.Fatal(err)
	}
	spawnPolecatForSling = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "Toast", ClonePath: fakeWorkDir,
			Branch: "polecat/Toast/x", FreshSpawn: true, BranchCreated: true}, nil
	}
	storeRawSlingMetadataFn = func(string, string, beadFieldUpdates) error { return nil }
	startSpawnedPolecatSessionFn = func(*SpawnedPolecatInfo) (string, error) { return "%1", nil }
	hookBeadWithRetryFn = func(string, string, string) error { return nil }
	findHookedFormulaSingletonFn = func(string, string, string) (*beads.Issue, error) { return nil, nil }
	acquirePolecatAdmissionFn = func(string, string, string, string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error) {
		return &polecatAdmissionHandle{disabled: true}, polecatCapacitySnapshot{}, nil
	}
	return townRoot
}

type rollbackCall struct{ beadID, convoyID string }

func recordRollbacks(t *testing.T) *[]rollbackCall {
	t.Helper()
	calls := &[]rollbackCall{}
	rollbackSlingArtifactsFn = func(spawnInfo *SpawnedPolecatInfo, beadID, _, convoyID string) {
		if spawnInfo == nil || spawnInfo.PolecatName != "Toast" {
			t.Errorf("rollback got spawnInfo %+v", spawnInfo)
		}
		*calls = append(*calls, rollbackCall{beadID: beadID, convoyID: convoyID})
	}
	return calls
}

var errInjected = errors.New("injected failure")

// --- runSlingFormula: same guard --------------------------------------------

func TestRunSlingFormulaRollsBackOnEveryPostSpawnExit(t *testing.T) {
	existing := func(string, string, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "gt-wisp-existing"}, nil
	}
	cases := []struct {
		name         string
		inject       func()
		wantErr      bool
		wantRollback bool
		wantBeadID   string
		wantErrSub   string
		wantBurned   []string // wisps the sling burned
		target       string   // default "gastown" (rig target)
	}{
		{name: "hooked-formula lookup fails", wantErrSub: "checking existing hooked formulas", wantErr: true, wantRollback: true, inject: func() {
			findHookedFormulaSingletonFn = func(string, string, string) (*beads.Issue, error) { return nil, errInjected }
		}},
		// A wisp still hooked to a just-spawned polecat's identity is stale:
		// it is burned and the sling dispatches fresh, instead of a "no-op"
		// that leaves it hooked to a polecat nobody starts.
		{name: "stale formula wisp is burned, then dispatch succeeds", wantBurned: []string{"gt-wisp-existing"}, inject: func() {
			findHookedFormulaSingletonFn = existing
		}},
		{name: "stale formula wisp burn fails", wantErrSub: "burning stale formula wisp", wantErr: true, wantRollback: true, inject: func() {
			findHookedFormulaSingletonFn = existing
			burnSlingWispFn = func(string, string) error { return errInjected }
		}},
		{name: "formula admission fails", target: "gastown/polecats/toast", wantErrSub: "injected failure", wantErr: true, wantRollback: true, inject: func() {
			resolveTargetAgentFn = func(string) (string, string, string, error) { return "", "", "", errors.New("no session") }
			acquirePolecatAdmissionFn = func(string, string, string, string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error) {
				return nil, polecatCapacitySnapshot{}, errInjected
			}
		}},
		{name: "cook fails", wantErrSub: "cooking formula", wantErr: true, wantRollback: true, inject: func() { _ = os.Setenv("BD_FAIL", "cook") }},
		{name: "wisp create fails", wantErrSub: "creating wisp", wantErr: true, wantRollback: true, inject: func() { _ = os.Setenv("BD_FAIL", "mol") }},
		{name: "wisp output unparseable", wantErrSub: "parsing wisp", wantErr: true, wantRollback: true, inject: func() { _ = os.Setenv("BD_WISP_OUT", "not json") }},
		{name: "hook fails", wantErrSub: "injected failure", wantErr: true, wantRollback: true, wantBeadID: "gt-wisp-new", wantBurned: []string{"gt-wisp-new"}, inject: func() {
			hookBeadWithRetryFn = func(string, string, string) error { return errInjected }
		}},
		{name: "session start fails", wantErrSub: "starting polecat session", wantErr: true, wantRollback: true, wantBeadID: "gt-wisp-new", wantBurned: []string{"gt-wisp-new"}, inject: func() {
			startSpawnedPolecatSessionFn = func(*SpawnedPolecatInfo) (string, error) { return "", errInjected }
		}},
		{name: "success commits", inject: func() {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupRollbackGuardTown(t)
			calls := recordRollbacks(t)
			var burned []string
			burnSlingWispFn = func(id, _ string) error { burned = append(burned, id); return nil }
			tc.inject()

			target := tc.target
			if target == "" {
				target = "gastown"
			}
			err := runSlingFormula(context.Background(), []string{"mol-test", target})
			if (err != nil) != tc.wantErr {
				t.Fatalf("runSlingFormula err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("runSlingFormula failed at the wrong step: %v (want %q)", err, tc.wantErrSub)
			}
			want := 0
			if tc.wantRollback {
				want = 1
			}
			if len(*calls) != want {
				t.Fatalf("rollback calls = %d, want %d (%+v)", len(*calls), want, *calls)
			}
			if want == 1 && (*calls)[0].beadID != tc.wantBeadID {
				t.Fatalf("rollback bead = %q, want %q", (*calls)[0].beadID, tc.wantBeadID)
			}
			if strings.Join(burned, ",") != strings.Join(tc.wantBurned, ",") {
				t.Fatalf("burned wisps = %v, want %v", burned, tc.wantBurned)
			}
		})
	}
}
