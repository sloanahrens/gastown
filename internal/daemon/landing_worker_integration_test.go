//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/slot"
)

// TestIntegrationPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot runs the real
// post-land runner against a real repository and a real container-gate hold,
// in a temp town whose slot nobody else contends for. The hold is taken in
// process by internal/land's CommandGate (gt-638go.12), so what this test can
// observe of it is the town's own slot history.
func TestIntegrationPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	townRoot := filepath.Join(root, "town")
	if err := os.Mkdir(townRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	stubSlotContainers()
	repo := filepath.Join(root, "repo")
	lwGit(t, root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "marker"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, repo, "add", ".")
	lwGit(t, repo, "commit", "-q", "-m", "landed")
	commit := lwGit(t, repo, "rev-parse", "HEAD")
	run := postLandRun(repo, filepath.Join(root, "work"), filepath.Join(root, "logs"), townRoot, "gastown", "origin", time.Minute)

	res := run(context.Background(), "cat marker && echo slow tier failed && exit 3", landworker.PostLand{BeadID: "gt-a", Commit: commit})
	if res.Err != nil || res.ExitCode != 3 || !strings.Contains(res.Tail, "landed") || !strings.Contains(res.Tail, "slow tier failed") {
		t.Fatalf("red run: %+v", res)
	}
	// gt-f2voh: the full output stays on disk, under the log root, for the
	// red-main bead to cite.
	if full, err := os.ReadFile(res.LogPath); err != nil || !strings.HasPrefix(res.LogPath, filepath.Join(root, "logs")) ||
		!strings.Contains(string(full), "slow tier failed") {
		t.Fatalf("full log %q: %q %v", res.LogPath, full, err)
	}
	res = run(context.Background(), "test -f marker", landworker.PostLand{BeadID: "gt-a", Commit: commit})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("green run: %+v", res)
	}
	// One hold per run, under the post-land role, either side of the
	// container-suite gate.
	history, err := slot.History(townRoot)
	if err != nil {
		t.Fatalf("reading the town's slot history: %v", err)
	}
	var holds int
	for _, e := range history {
		if e.Role == "gastown/post-land" && e.HeldS != nil {
			holds++
		}
	}
	if holds != 2 {
		t.Fatalf("slot history holds = %d, want one per run: %+v", holds, history)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "work")); len(entries) != 0 {
		t.Fatalf("worktrees left behind: %v", entries)
	}
}

// stubSlotContainers makes the container-gate's `docker ps` check answer "no
// containers" for this test binary, so an acquire in a temp town grants at
// once instead of polling for the full timeout against a host that happens to
// be running a dolt or testcontainers container (see
// slot.SetContainerListerForTest). Installed once, never restored: every
// caller in this package wants the same answer, and a per-test restore races
// under t.Parallel.
var stubSlotContainersOnce sync.Once

func stubSlotContainers() {
	stubSlotContainersOnce.Do(func() {
		slot.SetContainerListerForTest(func() ([]string, error) { return nil, nil })
	})
}

// TestIntegrationPostLandRunFetchesADirectPush is gt-p2rs0: a commit pushed
// straight to origin is not in the rig's bare repository, so the post-land
// run must fetch it before it adds a worktree there ("invalid reference").
func TestIntegrationPostLandRunFetchesADirectPush(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	origin, seed, bare := filepath.Join(root, "origin.git"), filepath.Join(root, "seed"), filepath.Join(root, ".repo.git")
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
	from := lwGit(t, seed, "rev-parse", "HEAD")
	lwGit(t, root, "clone", "-q", "--bare", origin, bare)
	if err := os.WriteFile(filepath.Join(seed, "pushed.txt"), []byte("direct\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, seed, "add", ".")
	lwGit(t, seed, "commit", "-q", "-m", "direct push")
	lwGit(t, seed, "push", "-q", "origin", "main")
	pushed := lwGit(t, seed, "rev-parse", "HEAD")
	townRoot := filepath.Join(root, "town")
	if err := os.Mkdir(townRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	stubSlotContainers()
	run := postLandRun(bare, filepath.Join(root, "work"), filepath.Join(root, "logs"), townRoot, "gastown", "origin", time.Minute)

	res := run(context.Background(), "cat pushed.txt", landworker.PostLand{Commit: pushed, Target: "main", Direct: true, From: from})
	if res.Err != nil || res.ExitCode != 0 || !strings.Contains(res.Tail, "direct") {
		t.Fatalf("post-land run at the direct push: %+v", res)
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
	if out := lwGit(t, root, "ls-remote", "--heads", origin, "refs/heads/polecat/"); out != "" {
		t.Fatalf("origin still holds a landed bead's polecat branches: %s", out)
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

// TestIntegrationRedMainRevertsTheCulpritThroughLand lands one piece of work
// on a green main, reports its post-landing run red, and checks the whole
// revert with real git: the revert branch is built and pushed from the rig's
// bare repo, the worker lands it through Land, main's tree is back to the
// last green one, and the culprit is reopened for rework.
func TestIntegrationRedMainRevertsTheCulpritThroughLand(t *testing.T) {
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
	green := lwGit(t, seed, "rev-parse", "HEAD")
	const branch = "polecat/opal/gt-cul+x1"
	lwGit(t, seed, "checkout", "-q", "-b", branch)
	// A Go file, so the diff can have moved the package this landing is
	// blamed for and red-main still reverts it (gt-40so9).
	if err := os.WriteFile(filepath.Join(seed, "b.go"), []byte("package b\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, seed, "add", ".")
	lwGit(t, seed, "commit", "-q", "-m", "feat: b")
	head := lwGit(t, seed, "rev-parse", "HEAD")
	lwGit(t, seed, "push", "-q", "origin", branch)
	lwGit(t, root, "clone", "-q", "--bare", origin, bare)
	lwGit(t, bare, "config", "user.email", "t@example.com")
	lwGit(t, bare, "config", "user.name", "T")

	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{ID: "gt-cul", Title: "b", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{land.LabelReadyToLand},
		Notes:  land.FormatReadyNote(land.Work{Branch: branch, Head: head, Target: "main", Worker: "opal"})})
	landings, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	workRoot, err := landingWorkRoot("", town, fmt.Sprintf("revert-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workRoot) })
	lander := &land.Lander{Repo: bare, WorkRoot: workRoot, Gate: &passGate{}, Reviewer: approve{},
		Beads: bd, Landings: landings, RangeChecks: []land.RangeCheck{land.AttributionCheck}}
	state := fileMainState{path: RedMainStatePath(town, "gastown")}
	var status []string
	redMain := &landworker.RedMain{Rig: "gastown", Beads: bd, Logf: t.Logf, State: state, Landings: landings,
		Revert: postLandRevert(bare, workRoot, "origin"),
		// The landing's real commit range, read out of the rig's own repo as
		// the daemon wires it: red-main only reverts a landing whose diff can
		// have moved what failed (gt-40so9).
		Diff: func(_ context.Context, rec land.LandingRecord) ([]string, error) {
			return git.NewGit(bare).DiffNameOnly(rec.Base, rec.LandedCommit)
		},
		Rerun: func(context.Context, string, string, landworker.PostLand) landworker.PostLandResult {
			return landworker.PostLandResult{ExitCode: 1, Tail: "--- FAIL: TestB"}
		},
		Status: func(line string) { status = append(status, line) }}
	w := &landworker.Worker{Rig: "gastown", Beads: bd, Remote: gitRemote{g: git.NewGit(bare), remote: "origin"}, Lander: lander,
		Landings: landings, Reverts: redMain, LandTimeout: time.Minute, Logf: t.Logf}

	redMain.Green(context.Background(), "make test-slow", landworker.PostLand{BeadID: "gt-prev", Commit: green}, landworker.PostLandResult{})
	if rep := w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("landing the culprit: %s", rep)
	}
	red := lwGit(t, origin, "rev-parse", "refs/heads/main")
	redMain.Red(context.Background(), "make test-slow", landworker.PostLand{BeadID: "gt-cul", Commit: red, Target: "main"},
		landworker.PostLandResult{ExitCode: 1, Packages: []land.PackageResult{{Package: "example.com/b"}}})
	if last := status[len(status)-1]; !strings.Contains(last, "reverting gt-cul as ") {
		t.Fatalf("status %q", last)
	}

	if rep := w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("landing the revert: %s", rep)
	}
	reverted := lwGit(t, origin, "rev-parse", "refs/heads/main")
	if lwGit(t, origin, "rev-parse", reverted+"^{tree}") != lwGit(t, origin, "rev-parse", green+"^{tree}") {
		t.Fatalf("main %s after the revert does not have the last green tree", reverted)
	}
	if recs, err := landings.Recent(1); err != nil || len(recs) != 1 || recs[0].LandedCommit != reverted || !strings.HasPrefix(recs[0].Branch, "revert/gt-cul-") {
		t.Fatalf("landings file %+v %v; want the revert's landing record", recs, err)
	}
	cul, _ := bd.Show("gt-cul")
	if cul.Status != "open" || !beads.HasLabel(cul, land.LabelRework) {
		t.Fatalf("culprit after the revert: status=%s labels=%v", cul.Status, cul.Labels)
	}
	if st, err := state.Load(); err != nil || st.LastGreen != green || st.LastRun != red {
		t.Fatalf("main state %+v %v", st, err)
	}
}

// TestIntegrationNewRigLandingWorkerUsesTheConfiguredLandingRemote: on a rig
// whose bare repository carries a remote matching its merge_queue.forgejo URL,
// every remote the landing path names — the lander, the branch-tip reads the
// worker makes through gitRemote, and the candidate gate that pushes land/<bead>
// — is that remote rather than an assumed origin (gt-fn9e6.9). A candidate
// pushed to origin would go to GitHub and the Forgejo gate would wait on a
// verdict that never comes.
func TestIntegrationNewRigLandingWorkerUsesTheConfiguredLandingRemote(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rigName = "testrig"
	rigPath := forgejoRigConfig(t, townRoot, rigName)
	bare := filepath.Join(rigPath, ".repo.git")
	lwGit(t, townRoot, "init", "-q", "--bare", "-b", "main", bare)
	lwGit(t, bare, "remote", "add", "origin", "https://github.com/acme/gastown.git")
	lwGit(t, bare, "remote", "add", "forgejo", "https://forgejo.example/gastown/gastown.git")

	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "forgejo-landing.env"), []byte("FORGEJO_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: notifyfake.New(),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
			Forgejo: &config.ForgejoWorkerConfig{TokenDir: tokenDir}}}}}

	w, err := d.newRigLandingWorker(rigName)
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	lander, ok := w.Lander.(*land.Lander)
	if !ok {
		t.Fatalf("Lander = %T, want *land.Lander", w.Lander)
	}
	if lander.Remote != "forgejo" {
		t.Errorf("Lander.Remote = %q, want forgejo (the URL's remote)", lander.Remote)
	}
	gate, ok := lander.Candidate.(*land.CandidateGate)
	if !ok {
		t.Fatalf("Candidate = %T; want the rig's Forgejo gate", lander.Candidate)
	}
	if gate.Remote != "forgejo" {
		t.Errorf("CandidateGate.Remote = %q, want forgejo so the candidate reaches the CI that gates it", gate.Remote)
	}
	remote, ok := w.Remote.(gitRemote)
	if !ok {
		t.Fatalf("Remote = %T, want the daemon's gitRemote", w.Remote)
	}
	if remote.remote != "forgejo" {
		t.Errorf("gitRemote.remote = %q, want forgejo so the branch-tip reads match what gt done pushed", remote.remote)
	}
}
