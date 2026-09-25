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
			chdirTempTown(t)
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", agent}}}
			sb := installRollbackFakes(t, rel)

			cleanupSpawnedPolecatWork(&SpawnedPolecatInfo{
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
			chdirTempTown(t)
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", toast}}}
			installRollbackFakes(t, rel)
			survivingWorkForBeadFn = func(string, string) (string, error) { return tc.branch, tc.err }

			info := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", FreshSpawn: true, originalHold: tc.orig}
			cleanupSpawnedPolecatWork(info, "gastown", "gt-abc", "", "")

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
	chdirTempTown(t)
	rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-abc": {"hooked", "gastown/polecats/granite"}}}
	installRollbackFakes(t, rel)

	cleanupSpawnedPolecatWork(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", FreshSpawn: true},
		"gastown", "gt-abc", "", "")
	if len(rel.released) != 0 {
		t.Fatalf("released a bead hooked to someone else: %v", rel.released)
	}
}

// The zero value is the safe one: an info built without provenance keeps the
// sandbox and the branch.
func TestCleanupSpawnedPolecatWorkZeroProvenanceKeepsEverything(t *testing.T) {
	chdirTempTown(t)
	rel := &fakeWorkReleaser{beads: map[string][2]string{}}
	sb := installRollbackFakes(t, rel)

	cleanupSpawnedPolecat(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", Branch: "feature/x"}, "gastown", "")
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
		collect                                                              func(*beadInfo, string, string) ([]string, error)
		burn                                                                 func([]string, string, string) error
		instantiate                                                          func(context.Context, string, string, string, string, string, bool, []string) (*FormulaOnBeadResult, error)
		lock                                                                 func(string, string) (func(), error)
		storeRaw                                                             func(string, string, beadFieldUpdates) error
		session                                                              func(*SpawnedPolecatInfo) (string, error)
		hook                                                                 func(string, string, string) error
		findFormula                                                          func(string, string, string) (*beads.Issue, error)
		admission                                                            func(string, string, string, string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error)
	}{slingNoConvoy, slingNoBoot, slingDryRun, slingHookRawBead, slingNoMerge, slingReviewOnly, slingForce, slingRalph,
		slingFormula, slingResumeBranch, slingBaseBranch, slingOnTarget, slingVars,
		spawnPolecatForSling, rollbackSlingArtifactsFn, collectExistingMoleculesForBeadFn, burnExistingMoleculesFn,
		instantiateFormulaOnBeadFn, tryAcquireSlingAssigneeLockFn, storeRawSlingMetadataFn, startSpawnedPolecatSessionFn,
		hookBeadWithRetryFn, findHookedFormulaSingletonFn, acquirePolecatAdmissionFn}
	t.Cleanup(func() {
		slingNoConvoy, slingNoBoot, slingDryRun, slingHookRawBead = prev.noConvoy, prev.noBoot, prev.dryRun, prev.hookRaw
		slingNoMerge, slingReviewOnly, slingForce, slingRalph = prev.noMerge, prev.reviewOnly, prev.force, prev.ralph
		slingFormula, slingResumeBranch, slingBaseBranch, slingOnTarget, slingVars = prev.formula, prev.resume, prev.base, prev.onTarget, prev.vars
		spawnPolecatForSling, rollbackSlingArtifactsFn = prev.spawn, prev.rollback
		collectExistingMoleculesForBeadFn, burnExistingMoleculesFn = prev.collect, prev.burn
		instantiateFormulaOnBeadFn, tryAcquireSlingAssigneeLockFn = prev.instantiate, prev.lock
		storeRawSlingMetadataFn, startSpawnedPolecatSessionFn = prev.storeRaw, prev.session
		hookBeadWithRetryFn, findHookedFormulaSingletonFn, acquirePolecatAdmissionFn = prev.hook, prev.findFormula, prev.admission
	})

	prevBurnWisp, prevConvoy, prevResolveAgent := burnSlingWispFn, createAutoConvoyFn, resolveTargetAgentFn
	prevClearLabels := clearOrphanEpisodeLabelsFn
	t.Cleanup(func() {
		burnSlingWispFn, createAutoConvoyFn, resolveTargetAgentFn = prevBurnWisp, prevConvoy, prevResolveAgent
		clearOrphanEpisodeLabelsFn = prevClearLabels
	})
	clearOrphanEpisodeLabelsFn = func(string, string, string) {}
	burnSlingWispFn = func(string, string) error { return nil }
	createAutoConvoyFn = func(string, string, bool, string, string, string, string) (string, error) {
		return "", errors.New("unexpected convoy create")
	}

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
	collectExistingMoleculesForBeadFn = func(*beadInfo, string, string) ([]string, error) { return nil, nil }
	burnExistingMoleculesFn = func([]string, string, string) error { return nil }
	instantiateFormulaOnBeadFn = func(_ context.Context, _, beadID, _, _, _ string, _ bool, _ []string) (*FormulaOnBeadResult, error) {
		return &FormulaOnBeadResult{WispRootID: "gt-wisp-new", BeadToHook: beadID}, nil
	}
	tryAcquireSlingAssigneeLockFn = func(string, string) (func(), error) { return func() {}, nil }
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

func TestRunSlingRollsBackOnEveryPostSpawnExit(t *testing.T) {
	const bead = "gt-abc123"
	cases := []struct {
		name         string
		inject       func()
		wantErr      bool
		wantRollback bool
		wantBeadID   string // "" = the failure came before the sling touched the bead
		wantErrSub   string // proves the run failed at the injected step, not earlier
		wantConvoy   string // the auto-convoy the rollback closes ("" = kept open, gt-yg24)
	}{
		{name: "molecule bond read fails", wantErrSub: "checking existing molecule bonds", wantErr: true, wantRollback: true, inject: func() {
			collectExistingMoleculesForBeadFn = func(*beadInfo, string, string) ([]string, error) { return nil, errInjected }
		}},
		{name: "stale molecule burn fails", wantErrSub: "burning stale molecules", wantErr: true, wantRollback: true, inject: func() {
			collectExistingMoleculesForBeadFn = func(*beadInfo, string, string) ([]string, error) { return []string{"gt-wisp-old"}, nil }
			burnExistingMoleculesFn = func([]string, string, string) error { return errInjected }
		}},
		{name: "live molecule refuses re-sling", wantErrSub: "already has 1 attached molecule", wantErr: true, wantRollback: true, inject: func() {
			_ = os.Setenv("BD_STATUS", "blocked") // unassigned + blocked: not an orphan molecule
			collectExistingMoleculesForBeadFn = func(*beadInfo, string, string) ([]string, error) { return []string{"gt-wisp-old"}, nil }
		}},
		{name: "formula instantiation fails", wantErrSub: "instantiating formula", wantErr: true, wantRollback: true, wantBeadID: bead, inject: func() {
			instantiateFormulaOnBeadFn = func(context.Context, string, string, string, string, string, bool, []string) (*FormulaOnBeadResult, error) {
				return nil, errInjected
			}
		}},
		{name: "assignee lock fails", wantErrSub: "serializing hook write", wantErr: true, wantRollback: true, wantBeadID: bead, inject: func() {
			tryAcquireSlingAssigneeLockFn = func(string, string) (func(), error) { return nil, errInjected }
		}},
		{name: "raw metadata store fails", wantErrSub: "storing raw sling metadata", wantErr: true, wantRollback: true, wantBeadID: bead, wantConvoy: "hq-cv-auto", inject: func() {
			slingHookRawBead, slingNoMerge = true, true
			storeRawSlingMetadataFn = func(string, string, beadFieldUpdates) error { return errInjected }
			slingNoConvoy = false
			createAutoConvoyFn = func(string, string, bool, string, string, string, string) (string, error) { return "hq-cv-auto", nil }
		}},
		{name: "hook fails with an auto-convoy keeps the convoy", wantErrSub: "injected failure", wantErr: true, wantRollback: true, wantBeadID: bead, inject: func() {
			slingHookRawBead = true
			slingNoConvoy = false
			createAutoConvoyFn = func(string, string, bool, string, string, string, string) (string, error) { return "hq-cv-auto", nil }
			hookBeadWithRetryFn = func(string, string, string) error { return errInjected }
		}},
		{name: "hook fails", wantErrSub: "injected failure", wantErr: true, wantRollback: true, wantBeadID: bead, inject: func() {
			hookBeadWithRetryFn = func(string, string, string) error { return errInjected }
		}},
		{name: "session start fails", wantErrSub: "starting polecat session", wantErr: true, wantRollback: true, wantBeadID: bead, inject: func() {
			startSpawnedPolecatSessionFn = func(*SpawnedPolecatInfo) (string, error) { return "", errInjected }
		}},
		{name: "success commits", inject: func() {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupRollbackGuardTown(t)
			calls := recordRollbacks(t)
			tc.inject()

			err := runSling(nil, []string{bead, "gastown"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("runSling err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("runSling failed at the wrong step: %v (want %q)", err, tc.wantErrSub)
			}
			if !tc.wantRollback {
				if len(*calls) != 0 {
					t.Fatalf("success path rolled back: %+v", *calls)
				}
				return
			}
			if len(*calls) != 1 {
				t.Fatalf("rollback calls = %d, want exactly 1 (%+v)", len(*calls), *calls)
			}
			if got := (*calls)[0].beadID; got != tc.wantBeadID {
				t.Fatalf("rollback bead = %q, want %q", got, tc.wantBeadID)
			}
			if got := (*calls)[0].convoyID; got != tc.wantConvoy {
				t.Fatalf("rollback convoy = %q, want %q", got, tc.wantConvoy)
			}
		})
	}
}

func TestRunSlingDryRunNeverRollsBack(t *testing.T) {
	setupRollbackGuardTown(t)
	calls := recordRollbacks(t)
	slingDryRun = true
	if err := runSling(nil, []string{"gt-abc123", "gastown"}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("dry run rolled back: %+v", *calls)
	}
}

// End to end through the real rollbackSlingArtifacts: a reused polecat whose
// session fails keeps its sandbox and branch, but gives the hook back and
// returns to idle. A fresh one is removed.
func TestRunSlingSessionFailureRespectsReuse(t *testing.T) {
	for _, reused := range []bool{true, false} {
		name := map[bool]string{true: "reused sandbox kept", false: "fresh sandbox removed"}[reused]
		t.Run(name, func(t *testing.T) {
			setupRollbackGuardTown(t)
			const bead = "gt-abc123"
			rel := &fakeWorkReleaser{beads: map[string][2]string{bead: {"hooked", "gastown/polecats/Toast"}}}
			sb := installRollbackFakes(t, rel)
			prevGet, prevCollect := getBeadInfoForRollback, collectExistingMoleculesForRollback
			getBeadInfoForRollback = func(string) (*beadInfo, error) { return &beadInfo{Status: "hooked"}, nil }
			collectExistingMoleculesForRollback = func(*beadInfo) []string { return nil }
			t.Cleanup(func() { getBeadInfoForRollback, collectExistingMoleculesForRollback = prevGet, prevCollect })

			spawnPolecatForSling = func(rigName string, _ SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
				return &SpawnedPolecatInfo{RigName: rigName, PolecatName: "Toast", Branch: "polecat/Toast/x",
					FreshSpawn: !reused, BranchCreated: true}, nil
			}
			startSpawnedPolecatSessionFn = func(*SpawnedPolecatInfo) (string, error) { return "", errInjected }

			if err := runSling(nil, []string{bead, "gastown"}); err == nil {
				t.Fatal("expected session failure")
			}
			if len(rel.released) != 1 || rel.released[0] != bead {
				t.Fatalf("hook not released: %v", rel.released)
			}
			if reused {
				if len(sb.removed) != 0 || len(sb.branches) != 0 {
					t.Fatalf("reused sandbox touched: removed %v, branches %v", sb.removed, sb.branches)
				}
				if len(rel.resets) != 1 {
					t.Fatalf("reused slot not reset to idle: %v", rel.resets)
				}
			} else if len(sb.removed) != 1 || len(sb.branches) != 1 {
				t.Fatalf("fresh sandbox not undone: removed %v, branches %v", sb.removed, sb.branches)
			}
		})
	}
}

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

// A sling that hooks the bead ends any witness orphan episode, so it clears
// the episode labels once, after the hook lands (gt-vm5g4). A sling that fails
// before the hook leaves them.
func TestRunSlingClearsOrphanEpisodeLabelsOnHook(t *testing.T) {
	const bead = "gt-abc123"
	for _, tc := range []struct {
		name   string
		inject func()
		want   []string
	}{
		{name: "success", inject: func() {}, want: []string{bead}},
		{name: "session fails after the hook", inject: func() {
			startSpawnedPolecatSessionFn = func(*SpawnedPolecatInfo) (string, error) { return "", errInjected }
		}, want: []string{bead}},
		{name: "hook fails", inject: func() {
			hookBeadWithRetryFn = func(string, string, string) error { return errInjected }
		}},
		{name: "fails before the hook", inject: func() {
			tryAcquireSlingAssigneeLockFn = func(string, string) (func(), error) { return nil, errInjected }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupRollbackGuardTown(t)
			_ = recordRollbacks(t)
			var got []string
			clearOrphanEpisodeLabelsFn = func(_, beadID, _ string) { got = append(got, beadID) }
			tc.inject()
			_ = runSling(nil, []string{bead, "gastown"})
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("label clears = %v, want %v", got, tc.want)
			}
		})
	}
}
