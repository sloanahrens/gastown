package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
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

// withSurvivingBranchForBead replaces the branch-lookup seam for a test.
func withSurvivingBranchForBead(t *testing.T, fn func(townRoot, beadID string) (string, bool)) {
	t.Helper()
	prev := survivingBranchForBeadFn
	t.Cleanup(func() { survivingBranchForBeadFn = prev })
	survivingBranchForBeadFn = fn
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
	withSurvivingBranchForBead(t, func(townRoot, beadID string) (string, bool) {
		return "polecat/pearl/gt-ibt8+mu72g5cz", true
	})

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
	withSurvivingBranchForBead(t, func(townRoot, beadID string) (string, bool) {
		return "polecat/pearl/gt-ibt8+mu72g5cz", true
	})

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

// TestSlingDeadAgentForcesWhenBranchUnknown pins the fail-open behaviour: if
// the surviving-branch lookup cannot answer (no repo, unreachable remote), the
// long-standing dead-holder auto-force must still work.
func TestSlingDeadAgentForcesWhenBranchUnknown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	_ = setupDeadHolderSlingFixture(t)
	withSurvivingBranchForBead(t, func(townRoot, beadID string) (string, bool) {
		return "", false
	})

	var err error
	stdout := captureStdout(t, func() {
		err = runSling(nil, []string{"gt-ibt8", "gastown/polecats/pearl"})
	})

	if err != nil && strings.Contains(err.Error(), "refusing to re-sling") {
		t.Fatalf("unknown branch state must not refuse, got: %v", err)
	}
	if !strings.Contains(stdout, "auto-forcing re-sling") {
		t.Errorf("expected auto-force when no branch is known, stdout: %q", stdout)
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

	branch, ok := survivingBranchForBead(townRoot, "gt-ibt8")
	if !ok {
		t.Fatalf("expected a surviving branch for gt-ibt8, got none")
	}
	if branch != "polecat/pearl/gt-ibt8+mu72g5cz" {
		t.Errorf("survivingBranchForBead() = %q, want the pushed branch", branch)
	}

	// A branch equal to main is not surviving work.
	if branch, ok := survivingBranchForBead(townRoot, "gt-empty"); ok {
		t.Errorf("a branch equal to main must not count as surviving work, got %q", branch)
	}

	// A bead with no branch reports unknown, not a false positive.
	if branch, ok := survivingBranchForBead(townRoot, "gt-nobranch"); ok {
		t.Errorf("expected no branch for gt-nobranch, got %q", branch)
	}

	// A prefix with no route reports unknown rather than guessing a path.
	if branch, ok := survivingBranchForBead(townRoot, "zz-unrouted"); ok {
		t.Errorf("expected no branch for unrouted prefix, got %q", branch)
	}
}
