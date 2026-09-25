package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// writeGastownRoutes maps the gt- prefix to the gastown rig, which is what
// survivingBranchForBead uses to find the rig's git repo.
func writeGastownRoutes(t *testing.T, townRoot string) {
	t.Helper()
	writeTestRoutes(t, townRoot, []beads.Route{{Prefix: "gt-", Path: "gastown/mayor/rig"}})
}

// setupDeadHolderSlingFixture wires the town fixtures the auto-force re-sling
// path needs: a hooked bead whose assignee's session is gone, and a stubbed
// dead-holder check.
func setupDeadHolderSlingFixture(t *testing.T) (townRoot string) {
	t.Helper()

	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeGastownRoutes(t, townRoot)

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	bdScript := `#!/bin/sh
set -e
cmd="$1"
shift || true
case "$cmd" in
  show)
    echo '[{"title":"Test issue","status":"hooked","assignee":"gastown/polecats/pearl","description":""}]'
    ;;
  update)
    exit 0
    ;;
esac
exit 0
`
	_ = writeBDStub(t, binDir, bdScript, "")

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_CREW", "")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevDeadFn := isHookedAgentDeadFn
	t.Cleanup(func() { isHookedAgentDeadFn = prevDeadFn })
	isHookedAgentDeadFn = func(assignee string) bool { return true }

	prevForce := slingForce
	prevNoConvoy := slingNoConvoy
	prevDryRun := slingDryRun
	prevResume := slingResumeBranch
	t.Cleanup(func() {
		slingForce = prevForce
		slingNoConvoy = prevNoConvoy
		slingDryRun = prevDryRun
		slingResumeBranch = prevResume
	})
	slingForce = false
	slingNoConvoy = true
	slingDryRun = true // avoid side effects from resolveTarget
	slingResumeBranch = ""

	return townRoot
}

// withSurvivingWork replaces the surviving-work seam for a test.
func withSurvivingWork(t *testing.T, branch string, err error) {
	t.Helper()
	prev := survivingWorkForBeadFn
	t.Cleanup(func() { survivingWorkForBeadFn = prev })
	survivingWorkForBeadFn = func(string, string) (string, error) { return branch, err }
}

// TestSlingDeadAgentRefusesWhenBranchSurvives is the guard half of the gt-3qfp
// fix. The holder's session is gone, so the stranded scan considers the bead
// re-slingable — but its branch is still on origin, so the work is preserved.
// Auto-forcing here is what spawned four polecats on gt-ibt8 and three on
// gt-da2x; the guard must refuse instead, and name the branch to resume.
func TestSlingDeadAgentRefusesWhenBranchSurvives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	_ = setupDeadHolderSlingFixture(t)
	withSurvivingWork(t, "polecat/pearl/gt-ibt8+mu72g5cz", nil)

	var err error
	stdout := captureStdout(t, func() {
		err = runSling(nil, []string{"gt-ibt8", "gastown/polecats/pearl"})
	})

	if err == nil {
		t.Fatal("expected refusal when a surviving branch exists, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to re-sling gt-ibt8") {
		t.Errorf("expected refusal error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "polecat/pearl/gt-ibt8+mu72g5cz") {
		t.Errorf("refusal must name the surviving branch, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--branch") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal must offer both resume and discard paths, got: %v", err)
	}
	if strings.Contains(stdout, "auto-forcing re-sling") {
		t.Errorf("must not auto-force when work is preserved, stdout: %q", stdout)
	}
}

// TestSlingDeadAgentResumesWithBranchFlag verifies the escape hatch: passing
// --branch is an explicit decision to resume the preserved work, so the guard
// steps aside and the normal auto-force path runs.
func TestSlingDeadAgentResumesWithBranchFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	_ = setupDeadHolderSlingFixture(t)
	slingResumeBranch = "polecat/pearl/gt-ibt8+mu72g5cz"
	withSurvivingWork(t, "polecat/pearl/gt-ibt8+mu72g5cz", nil)

	var err error
	stdout := captureStdout(t, func() {
		err = runSling(nil, []string{"gt-ibt8", "gastown/polecats/pearl"})
	})

	if err != nil && strings.Contains(err.Error(), "refusing to re-sling") {
		t.Fatalf("--branch must bypass the surviving-branch guard, got: %v", err)
	}
	if !strings.Contains(stdout, "auto-forcing re-sling") {
		t.Errorf("expected the dead-holder auto-force path to run, stdout: %q", stdout)
	}
}

// TestSlingDeadAgentRefusesWhenSurvivalUnknown: when surviving work cannot
// be verified (unreachable remote, timeout), the guard refuses rather than
// risk a second polecat from main over preserved work; --branch or --force are
// the ways forward.
func TestSlingDeadAgentRefusesWhenSurvivalUnknown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	_ = setupDeadHolderSlingFixture(t)
	withSurvivingWork(t, "", errors.New("origin unreachable"))

	var err error
	stdout := captureStdout(t, func() {
		err = runSling(nil, []string{"gt-ibt8", "gastown/polecats/pearl"})
	})
	if err == nil || !strings.Contains(err.Error(), "cannot verify surviving work (origin unreachable)") {
		t.Fatalf("want a cannot-verify refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "resume with --branch or override with --force") {
		t.Fatalf("refusal must name both ways forward, got %v", err)
	}
	if strings.Contains(stdout, "auto-forcing re-sling") {
		t.Errorf("must not auto-force on an unknown answer, stdout: %q", stdout)
	}
}

// TestSlingDeadAgentForcesWhenNothingToProtect: no rig repo, or a bead that
// routes to no rig, means there is no branch to protect, so the dead-holder
// auto-force still runs. So does an explicit --force on an unknown answer.
func TestSlingDeadAgentForcesWhenNothingToProtect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	for _, tc := range []struct {
		name  string
		err   error
		force bool
	}{
		{name: "no rig repo", err: polecat.ErrNoRigRepo},
		{name: "routes to no rig", err: fmt.Errorf("gt-ibt8: %w", errBeadRoutesToNoRig)},
		{name: "no surviving work"},
		{name: "explicit --force on an unknown answer", err: errors.New("origin unreachable"), force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = setupDeadHolderSlingFixture(t)
			withSurvivingWork(t, "", tc.err)
			slingForce = tc.force

			var err error
			stdout := captureStdout(t, func() {
				err = runSling(nil, []string{"gt-ibt8", "gastown/polecats/pearl"})
			})
			if err != nil && strings.Contains(err.Error(), "refusing to re-sling") {
				t.Fatalf("must not refuse, got: %v", err)
			}
			if !tc.force && !strings.Contains(stdout, "auto-forcing re-sling") {
				t.Errorf("expected the dead-holder auto-force, stdout: %q", stdout)
			}
		})
	}
}

// TestSurvivingBranchForBead_ResolvesRigRepo exercises the real wiring end to
// end: routes.jsonl -> rig name -> <town>/<rig>/.repo.git -> origin refs. A
// wrong rig root here would silently disable the whole guard, so it is worth
// the git fixture.
func TestSurvivingBranchForBead_ResolvesRigRepo(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	townRoot := t.TempDir()
	writeGastownRoutes(t, townRoot)

	originDir := filepath.Join(townRoot, "origin.git")
	runGit(t, townRoot, "init", "--bare", originDir)

	seed := filepath.Join(townRoot, "seed")
	runGit(t, townRoot, "init", "--initial-branch=main", seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "f.txt")
	runGit(t, seed, "commit", "-m", "base")
	// Surviving work (the shared predicate, polecat.WorkSurvival): a branch
	// with a patch that is not on main. A branch equal to main carries no
	// work and must not block a re-sling.
	runGit(t, seed, "branch", "polecat/agate/gt-empty+mu72g5cz")
	runGit(t, seed, "checkout", "-q", "-b", "polecat/pearl/gt-ibt8+mu72g5cz")
	if err := os.WriteFile(filepath.Join(seed, "work.txt"), []byte("work\n"), 0644); err != nil {
		t.Fatalf("write work file: %v", err)
	}
	runGit(t, seed, "add", "work.txt")
	runGit(t, seed, "commit", "-m", "work (gt-ibt8)")
	runGit(t, seed, "remote", "add", "origin", originDir)
	runGit(t, seed, "push", "origin", "main", "polecat/pearl/gt-ibt8+mu72g5cz", "polecat/agate/gt-empty+mu72g5cz")

	// The rig root the daemon and sling both use: <town>/<rigName>.
	rigRoot := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigRoot, 0755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	bare := filepath.Join(rigRoot, ".repo.git")
	runGit(t, townRoot, "init", "--bare", bare)
	runGit(t, bare, "remote", "add", "origin", originDir)

	branch, err := survivingWorkForBead(townRoot, "gt-ibt8")
	if err != nil {
		t.Fatalf("survivingWorkForBead(gt-ibt8): %v", err)
	}
	if branch != "polecat/pearl/gt-ibt8+mu72g5cz" {
		t.Errorf("survivingWorkForBead() = %q, want the pushed branch", branch)
	}

	// A branch equal to main is not surviving work.
	if branch, err := survivingWorkForBead(townRoot, "gt-empty"); err != nil || branch != "" {
		t.Errorf("a branch equal to main must not count as surviving work, got %q (%v)", branch, err)
	}

	// A bead with no branch reports unknown, not a false positive.
	if branch, err := survivingWorkForBead(townRoot, "gt-nobranch"); err != nil || branch != "" {
		t.Errorf("expected no branch for gt-nobranch, got %q", branch)
	}

	// A prefix with no route reports unknown rather than guessing a path.
	if branch, err := survivingWorkForBead(townRoot, "zz-unrouted"); !errors.Is(err, errBeadRoutesToNoRig) || branch != "" {
		t.Errorf("expected no branch for unrouted prefix, got %q", branch)
	}
}

// executeSling's dead-holder auto-force (batch sling, convoy and epic
// feeders, scheduler dispatch) runs the same surviving-work guard as runSling
// (gt-vm5g4): it refuses when the work survives or survival is unknown, unless
// --force or --branch, and the refusal is recognizable as errReslingRefused.
func TestExecuteSlingDeadHolderRunsSurvivalGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	const branch = "polecat/pearl/gt-ibt8+mu72g5cz"
	cases := []struct {
		name        string
		branch      string
		survErr     error
		force       bool
		resume      string
		wantRefused bool
		wantCalls   int
	}{
		{name: "work survives", branch: branch, wantRefused: true, wantCalls: 1},
		{name: "survival unknown", survErr: errors.New("git ls-remote timed out"), wantRefused: true, wantCalls: 1},
		{name: "nothing survives", wantCalls: 1},
		{name: "no rig repo", survErr: polecat.ErrNoRigRepo, wantCalls: 1},
		{name: "explicit force skips the guard", branch: branch, force: true},
		{name: "resume branch skips the guard", branch: branch, resume: branch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := setupDeadHolderSlingFixture(t)
			calls := 0
			prev := survivingWorkForBeadFn
			t.Cleanup(func() { survivingWorkForBeadFn = prev })
			survivingWorkForBeadFn = func(string, string) (string, error) {
				calls++
				return tc.branch, tc.survErr
			}

			var (
				result *SlingResult
				err    error
			)
			stdout := captureStdout(t, func() {
				result, err = executeSling(SlingParams{
					BeadID:             "gt-ibt8",
					RigName:            "norig", // unresolvable: stops right after the guard, before any side effect
					Force:              tc.force,
					ResumeBranch:       tc.resume,
					TownRoot:           townRoot,
					SkipDuplicateCheck: true,
				})
			})
			if calls != tc.wantCalls {
				t.Fatalf("survival checks = %d, want %d", calls, tc.wantCalls)
			}
			if err == nil {
				t.Fatal("want an error (refusal, or the unresolvable rig after the guard)")
			}
			wrapped := fmt.Errorf("sling failed: %w", err) // as dispatchSingleBead wraps it
			if got := errors.Is(wrapped, errReslingRefused); got != tc.wantRefused {
				t.Fatalf("refused = %v, want %v (err: %v)", got, tc.wantRefused, err)
			}
			if tc.wantRefused {
				if !strings.Contains(err.Error(), "refusing to re-sling gt-ibt8") || result == nil || result.ErrMsg != err.Error() {
					t.Fatalf("refusal text/result wrong: err=%v result=%+v", err, result)
				}
				if tc.branch != "" && !strings.Contains(err.Error(), tc.branch) {
					t.Fatalf("refusal must name the surviving branch: %v", err)
				}
				if strings.Contains(stdout, "auto-forcing") {
					t.Fatalf("refused sling still auto-forced:\n%s", stdout)
				}
				return
			}
			if !strings.Contains(err.Error(), "cannot resolve target rig") {
				t.Fatalf("want to reach the rig check after the guard, got %v", err)
			}
			if !tc.force && !strings.Contains(stdout, "auto-forcing dispatch") {
				t.Fatalf("dead holder with nothing to protect must auto-force:\n%s", stdout)
			}
		})
	}
}

// Convoy and epic feeders count a resling refusal as a deferral: not a
// success, not a failed attempt, and a run of only deferrals is not an error.
func TestFeederDispatchTallyTreatsReslingRefusalAsDeferral(t *testing.T) {
	refusal := &reslingRefusal{msg: "refusing to re-sling gt-a: ..."}
	cases := []struct {
		name    string
		errs    []error
		wantErr string
	}{
		{name: "all deferred", errs: []error{refusal, fmt.Errorf("wrapped: %w", refusal)}},
		{name: "deferred and dispatched", errs: []error{refusal, nil}},
		{name: "deferred and failed", errs: []error{refusal, errors.New("spawn failed")}, wantErr: "all 1 dispatch attempts failed for convoy hq-cv-1"},
		{name: "all failed", errs: []error{errors.New("x"), errors.New("y")}, wantErr: "all 2 dispatch attempts failed for convoy hq-cv-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tally feederDispatchTally
			var err error
			out := captureStdout(t, func() {
				for i, e := range tc.errs {
					ok := tally.record(fmt.Sprintf("gt-%d", i), e)
					if ok != (e == nil) {
						t.Errorf("record(%v) = %v", e, ok)
					}
				}
				err = tally.result("convoy", "hq-cv-1")
			})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if errors.Is(tc.errs[0], errReslingRefused) && strings.Contains(out, "✗ gt-0") {
				t.Fatalf("a refusal was printed as a failure:\n%s", out)
			}
		})
	}
}

// The scheduler leaves a refused dispatch queued without recording a failure,
// so a preserved-work bead never trips the circuit breaker.
func TestCapacityDispatchDeferralRecognizesReslingRefusal(t *testing.T) {
	refusal := &reslingRefusal{msg: "refusing to re-sling gt-a: ..."}
	if _, ok := capacityDispatchDeferral(fmt.Errorf("sling failed: %w", refusal)); !ok {
		t.Fatal("a wrapped resling refusal must be a deferral")
	}
	if _, ok := capacityDispatchDeferral(fmt.Errorf("sling failed: %w", &polecatCapacityAdmissionError{})); !ok {
		t.Fatal("a capacity admission refusal must stay a deferral")
	}
	if _, ok := capacityDispatchDeferral(errors.New("sling failed: spawn failed")); ok {
		t.Fatal("an ordinary failure must not be a deferral")
	}
}
