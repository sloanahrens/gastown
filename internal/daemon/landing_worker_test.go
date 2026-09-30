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

type passGate struct{ dirs []string }

func (g *passGate) Run(_ context.Context, dir string) land.GateResult {
	g.dirs = append(g.dirs, dir)
	return land.GateResult{Passed: true, Steps: []land.StepResult{{Name: "gate"}}}
}

type approve struct{}

func (approve) Review(context.Context, string, string, string) (land.Verdict, error) {
	return land.Verdict{Verdict: land.VerdictApprove, Score: 0.9}, nil
}

// TestLandingWorkerLandsFromTheRigBareRepo runs one worker pass the way the
// daemon wires it: Land and the remote read from a bare clone (the rig's
// .repo.git), and the branch tip, not the submitted head, lands.
func TestLandingWorkerLandsFromTheRigBareRepo(t *testing.T) {
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

func TestLandingWorkerConfigDefaults(t *testing.T) {
	if IsPatrolEnabled(nil, "landing_worker") {
		t.Fatal("landing_worker enabled with no config; it must be opt-in")
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if IsPatrolEnabled(cfg, "landing_worker") {
		t.Fatal("landing_worker enabled with no entry")
	}
	if landingWorkerInterval(cfg) != defaultLandingWorkerInterval || landingWorkerLandTimeout(cfg) != landworker.DefaultLandTimeout {
		t.Fatal("defaults not applied")
	}
	cfg.Patrols.LandingWorker = &LandingWorkerConfig{Enabled: true, IntervalStr: "2m", LandTimeoutStr: "45m", Rigs: []string{"gastown"}}
	if !IsPatrolEnabled(cfg, "landing_worker") || landingWorkerInterval(cfg) != 2*time.Minute || landingWorkerLandTimeout(cfg) != 45*time.Minute {
		t.Fatal("configured values not applied")
	}
	if got := landingWorkerRigs(cfg, []string{"beads", "gastown", "hm"}); len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("rigs = %v", got)
	}
}

func TestPruneLandingLogs(t *testing.T) {
	root := t.TempDir()
	old, fresh := filepath.Join(root, "land-old"), filepath.Join(root, "land-new")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-landingLogRetention - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	pruneLandingLogs(root, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old log dir kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh log dir removed")
	}
}

func TestLandingReviewOnByDefault(t *testing.T) {
	t.Parallel()
	if !landingReviewEnabled(nil) || !landingReviewEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Enabled: true}}}) {
		t.Fatal("om review must be on unless explicitly disabled")
	}
	off := false
	if landingReviewEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Review: &off}}}) {
		t.Fatal("review:false did not disable the review")
	}
}

func TestResolveOMPath(t *testing.T) {
	t.Parallel()
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	onPath := func(string) (string, error) { return "/usr/local/bin/om", nil }
	if got := resolveOMPath("/opt/om", onPath, "/Users/x"); got != "/opt/om" {
		t.Errorf("configured: %s", got)
	}
	if got := resolveOMPath("", onPath, "/Users/x"); got != "/usr/local/bin/om" {
		t.Errorf("on PATH: %s", got)
	}
	if got := resolveOMPath("", missing, "/Users/x"); got != "/Users/x/go/bin/om" {
		t.Errorf("fallback: %s", got)
	}
}

func TestRigPostLandCommandReadsRigSettingsOnly(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if got := rigPostLandCommand(rigPath); got != "" {
		t.Fatalf("no settings: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"rig-settings","version":1,"merge_queue":{"gate":"make gate","post_land_command":"make test-slow"}}`
	if err := os.WriteFile(filepath.Join(rigPath, "settings", "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rigPostLandCommand(rigPath); got != "make test-slow" {
		t.Fatalf("post_land_command = %q", got)
	}
}

// TestPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot runs the real
// post-land runner against a real repository, with a stub gt whose "slot
// run" only strips its own arguments.
func TestPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot(t *testing.T) {
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

// The landing work root must never be under the town root: internal/git
// refuses worktree targets there, which failed every landing on 2026-09-30.
func TestLandingWorkRootIsOutsideTheTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	root, err := landingWorkRoot("", town, "gastown")
	if err != nil {
		t.Fatalf("default work root: %v", err)
	}
	if landingPathWithin(root, town) {
		t.Fatalf("default work root %s is under the town root %s", root, town)
	}
	if filepath.Base(root) != "gastown" || !strings.Contains(root, fmt.Sprintf("gt-landing-%d", os.Getuid())) {
		t.Fatalf("default work root %s is not uid- and rig-scoped", root)
	}

	for _, bad := range []string{filepath.Join(town, ".runtime", "landing-work"), town} {
		if _, err := landingWorkRoot(bad, town, "gastown"); err == nil || !strings.Contains(err.Error(), "under the town root") {
			t.Errorf("work_root %s: err = %v; want a refusal naming the town root", bad, err)
		}
	}
	if _, err := landingWorkRoot("relative/dir", town, "gastown"); err == nil {
		t.Error("a relative work_root was accepted")
	}
	outside := t.TempDir()
	if got, err := landingWorkRoot(outside, town, "gastown"); err != nil || got != filepath.Join(outside, "gastown") {
		t.Errorf("work_root outside the town: %s, %v", got, err)
	}
}

// A work root spelled through a symlink into the town root is refused too.
func TestLandingWorkRootRefusesASymlinkIntoTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(town, link); err != nil {
		t.Fatal(err)
	}
	if _, err := landingWorkRoot(filepath.Join(link, "work"), town, "gastown"); err == nil {
		t.Fatal("a work_root reaching the town root through a symlink was accepted")
	}
}

// The git guard itself accepts a worktree at the default work root.
func TestLandingWorkRootPassesTheGitGuard(t *testing.T) {
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
