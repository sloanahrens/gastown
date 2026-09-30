//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// TestIntegrationPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot runs the real
// post-land runner against a real repository, with a stub gt whose "slot
// run" only strips its own arguments. The runner's shell and gt run inside
// internal/land's CommandGate, which has no seam this package can reach.
func TestIntegrationPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	lwGit(t, root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "marker"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, repo, "add", ".")
	lwGit(t, repo, "commit", "-q", "-m", "landed")
	commit := lwGit(t, repo, "rev-parse", "HEAD")
	stub := filepath.Join(root, "gt")
	slotLog := filepath.Join(root, "slot.log")
	script := "#!/bin/sh\necho \"$@\" >> " + slotLog + "\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := postLandRun(repo, filepath.Join(root, "work"), filepath.Join(root, "logs"), stub, "gastown", time.Minute)

	res := run(context.Background(), "cat marker && echo slow tier failed && exit 3", landworker.PostLand{BeadID: "gt-a", Commit: commit})
	if res.Err != nil || res.ExitCode != 3 || !strings.Contains(res.Tail, "landed") || !strings.Contains(res.Tail, "slow tier failed") {
		t.Fatalf("red run: %+v", res)
	}
	res = run(context.Background(), "test -f marker", landworker.PostLand{BeadID: "gt-a", Commit: commit})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("green run: %+v", res)
	}
	data, err := os.ReadFile(slotLog)
	if err != nil || strings.Count(string(data), "slot run --role gastown/post-land --") != 2 {
		t.Fatalf("slot wrapper calls: %q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "work")); len(entries) != 0 {
		t.Fatalf("worktrees left behind: %v", entries)
	}
}

func lwGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestIntegrationLandingWorkerLandsFromTheRigBareRepo runs one worker pass the way the
// daemon wires it: Land and the remote read from a bare clone (the rig's
// .repo.git), and the branch tip, not the submitted head, lands.
func TestIntegrationLandingWorkerLandsFromTheRigBareRepo(t *testing.T) {
	root := t.TempDir()
	origin, seed, bare, town := filepath.Join(root, "origin.git"), filepath.Join(root, "seed"), filepath.Join(root, ".repo.git"), filepath.Join(root, "town")
	lwGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	lwGit(t, root, "clone", "-q", origin, seed)
	lwGit(t, seed, "config", "core.hooksPath", "/dev/null")
	lwGit(t, seed, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, seed, "add", ".")
	lwGit(t, seed, "commit", "-q", "-m", "seed")
	lwGit(t, seed, "push", "-q", "origin", "main")
	const branch = "polecat/opal/gt-abc+x1"
	lwGit(t, seed, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(seed, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, seed, "add", ".")
	lwGit(t, seed, "commit", "-q", "-m", "feat: b (Claude Code hooks mention is fine)")
	submitted := lwGit(t, seed, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(seed, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, seed, "add", ".")
	lwGit(t, seed, "commit", "-q", "-m", "feat: c")
	tip := lwGit(t, seed, "rev-parse", "HEAD")
	lwGit(t, seed, "push", "-q", "origin", branch)
	lwGit(t, root, "clone", "-q", "--bare", origin, bare)
	lwGit(t, bare, "config", "user.email", "t@example.com")
	lwGit(t, bare, "config", "user.name", "T")

	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{ID: "gt-abc", Title: "b", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{land.LabelReadyToLand},
		Notes:  land.FormatReadyNote(land.Work{Branch: branch, Head: submitted, Target: "main", Worker: "opal"})})
	landings, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	gate := &passGate{}
	// The production work root, not a path under the (fake) town.
	workRoot, err := landingWorkRoot("", town, fmt.Sprintf("e2e-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workRoot) })
	lander := &land.Lander{Repo: bare, WorkRoot: workRoot, Gate: gate, Reviewer: approve{},
		Beads: bd, Landings: landings, RangeChecks: []land.RangeCheck{land.AttributionCheck}}
	var cleared []string
	w := &landworker.Worker{Rig: "gastown", Beads: bd, Remote: gitRemote{g: git.NewGit(bare), remote: "origin"}, Lander: lander, Landings: landings,
		LandTimeout: time.Minute, Logf: t.Logf, ClearIntent: func(w land.Work) error { cleared = append(cleared, w.Worker); return nil }}

	rep := w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("pass: %s", rep)
	}
	mainTip := lwGit(t, origin, "rev-parse", "refs/heads/main")
	if lwGit(t, origin, "rev-parse", mainTip+"^2") != tip {
		t.Fatalf("origin/main %s does not merge the branch tip %s", mainTip, tip)
	}
	is, _ := bd.Show("gt-abc")
	if is.Status != "closed" || beads.HasLabel(is, land.LabelReadyToLand) || !strings.Contains(is.Notes, land.LandingNoteMarker) {
		t.Fatalf("bead after landing: status=%s labels=%v", is.Status, is.Labels)
	}
	rec, ok, err := landings.LatestForBead("gt-abc")
	if err != nil || !ok || rec.LandedCommit != mainTip || rec.Head != tip || rec.Route != "" && rec.Route != "daemon" {
		t.Fatalf("landing record %+v ok=%v err=%v", rec, ok, err)
	}
	if len(cleared) != 1 || cleared[0] != "opal" {
		t.Fatalf("intent cleared for %v", cleared)
	}
	// The remote agrees the landed commit is on main, so a re-run would only repair.
	if on, err := (gitRemote{g: git.NewGit(bare), remote: "origin"}).Contains("main", mainTip); err != nil || !on {
		t.Fatalf("Contains(main, landed) = %v, %v", on, err)
	}
	if on, _ := (gitRemote{g: git.NewGit(bare), remote: "origin"}).Contains("main", strings.Repeat("e", 40)); on {
		t.Fatal("an unknown commit reads as on main")
	}
}

// The git guard itself (internal/git's town-root check, run by real git
// commands) accepts a worktree at the default work root.
func TestIntegrationLandingWorkRootPassesTheGitGuard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	lwGit(t, root, "init", "-q", "-b", "main", repo)
	lwGit(t, repo, "commit", "-q", "--allow-empty", "-m", "c")
	workRoot, err := landingWorkRoot("", filepath.Join(root, "town"), fmt.Sprintf("guardtest-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workRoot) })
	if err := ensurePrivateDir(workRoot); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(workRoot, "wt")
	if err := git.NewGit(repo).WorktreeAddDetached(dir, "HEAD"); err != nil {
		t.Fatalf("worktree at the default work root: %v", err)
	}
	if info, err := os.Stat(workRoot); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("work root mode: %v %v", info.Mode(), err)
	}
}

type passGate struct{ dirs []string }

func (g *passGate) Run(_ context.Context, dir string) land.GateResult {
	g.dirs = append(g.dirs, dir)
	return land.GateResult{Passed: true, Steps: []land.StepResult{{Name: "gate"}}}
}

type approve struct{}

func (approve) Review(context.Context, string, string, string) (land.Verdict, error) {
	return land.Verdict{Verdict: land.VerdictApprove, Score: 0.9}, nil
}
