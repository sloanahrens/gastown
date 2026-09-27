package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckpointDogInterval_Default(t *testing.T) {
	interval := checkpointDogInterval(nil)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_NilPatrols(t *testing.T) {
	config := &DaemonPatrolConfig{}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_NilCheckpointDog(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{},
	}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_Configured(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "5m",
			},
		},
	}
	interval := checkpointDogInterval(config)
	if interval != 5*time.Minute {
		t.Errorf("expected 5m, got %v", interval)
	}
}

func TestCheckpointDogInterval_InvalidFallsBack(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "not-a-duration",
			},
		},
	}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval for invalid config, got %v", interval)
	}
}

func TestCheckpointDogInterval_ZeroFallsBack(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "0s",
			},
		},
	}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval for zero config, got %v", interval)
	}
}

func TestCheckpointDogEnabled(t *testing.T) {
	// Nil config → disabled (opt-in patrol)
	if IsPatrolEnabled(nil, "checkpoint_dog") {
		t.Error("expected checkpoint_dog disabled for nil config")
	}

	// Explicitly enabled
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled: true,
			},
		},
	}
	if !IsPatrolEnabled(config, "checkpoint_dog") {
		t.Error("expected checkpoint_dog enabled")
	}

	// Explicitly disabled
	config.Patrols.CheckpointDog.Enabled = false
	if IsPatrolEnabled(config, "checkpoint_dog") {
		t.Error("expected checkpoint_dog disabled when Enabled=false")
	}
}

func TestResolveCheckpointWorkDir_NestedLayout(t *testing.T) {
	// New polecat layout: polecats/<name>/<rigName>/.git is the worktree.
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "alice"
	polecatsDir := filepath.Join(tmp, "polecats")
	worktree := filepath.Join(polecatsDir, polecat, rig)
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != worktree {
		t.Errorf("got %q, want %q", got, worktree)
	}
}

func TestResolveCheckpointWorkDir_LegacyFlatLayout(t *testing.T) {
	// Legacy layout: polecats/<name>/.git directly. polecat.Manager still
	// recognizes this; checkpoint_dog must too rather than silently skip.
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "bob"
	polecatsDir := filepath.Join(tmp, "polecats")
	worktree := filepath.Join(polecatsDir, polecat)
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != worktree {
		t.Errorf("got %q, want %q (legacy flat layout)", got, worktree)
	}
}

func TestResolveCheckpointWorkDir_NoGitNeitherLevel(t *testing.T) {
	// Critical regression case: polecat container exists but has no .git
	// at either level. Function MUST return "" so the caller skips, NOT
	// fall back to a parent dir (which would have the workspace's .git
	// and cause the wrong-branch checkpoint bug this code prevents).
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "carol"
	polecatsDir := filepath.Join(tmp, "polecats")
	if err := os.MkdirAll(filepath.Join(polecatsDir, polecat, rig), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Simulate top-level workspace .git that git would walk up to find.
	// resolveCheckpointWorkDir must NOT return a path that lets git walk
	// to this — it should return "" so the caller skips entirely.
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("setup parent .git: %v", err)
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != "" {
		t.Errorf("got %q, want empty (skip — no polecat-level .git)", got)
	}
}

func TestResolveCheckpointWorkDir_PrefersNestedOverFlat(t *testing.T) {
	// If both levels have .git (transitional state during a migration),
	// prefer the nested (newer) layout.
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "dave"
	polecatsDir := filepath.Join(tmp, "polecats")
	flat := filepath.Join(polecatsDir, polecat)
	nested := filepath.Join(flat, rig)
	for _, d := range []string{flat, nested} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatalf("setup %s: %v", d, err)
		}
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != nested {
		t.Errorf("got %q, want nested %q", got, nested)
	}
}

func TestIsGitWorktree(t *testing.T) {
	tmp := t.TempDir()
	if isGitWorktree(tmp) {
		t.Error("empty dir should not be a worktree")
	}
	// .git as directory (full clone)
	dirGit := filepath.Join(tmp, "fullclone")
	if err := os.MkdirAll(filepath.Join(dirGit, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitWorktree(dirGit) {
		t.Error(".git directory should count as worktree")
	}
	// .git as file (linked worktree — git uses a file pointing to commondir)
	fileGit := filepath.Join(tmp, "linked")
	if err := os.MkdirAll(fileGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileGit, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isGitWorktree(fileGit) {
		t.Error(".git file (linked worktree) should count as worktree")
	}
}

func TestCheckpointWorktreeExcludesNestedRuntimeArtifacts(t *testing.T) {
	workDir := t.TempDir()
	mustRunGit(t, workDir, "init")
	mustRunGit(t, workDir, "config", "user.name", "Checkpoint Dog")
	mustRunGit(t, workDir, "config", "user.email", "checkpoint@example.com")

	if err := os.MkdirAll(filepath.Join(workDir, "src"), 0o755); err != nil {
		t.Fatalf("setup src: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "web", ".beads"), 0o755); err != nil {
		t.Fatalf("setup nested runtime dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "src", "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "web", ".beads", "redirect"), []byte("before\n"), 0o644); err != nil {
		t.Fatalf("write runtime file: %v", err)
	}
	mustRunGit(t, workDir, "add", "src/app.go", "web/.beads/redirect")
	mustRunGit(t, workDir, "commit", "-m", "initial")

	if err := os.WriteFile(filepath.Join(workDir, "src", "app.go"), []byte("package main\n// checkpoint me\n"), 0o644); err != nil {
		t.Fatalf("modify source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "web", ".beads", "redirect"), []byte("after\n"), 0o644); err != nil {
		t.Fatalf("modify runtime file: %v", err)
	}

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}

	if got := strings.TrimSpace(mustRunGit(t, workDir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")); got != "src/app.go" {
		t.Fatalf("checkpoint commit changed %q, want only src/app.go", got)
	}
	if got := strings.TrimSpace(mustRunGit(t, workDir, "diff", "--cached", "--name-only")); got != "" {
		t.Fatalf("runtime artifact remained staged: %q", got)
	}
	if got := strings.TrimSpace(mustRunGit(t, workDir, "diff", "--", "web/.beads/redirect")); got == "" {
		t.Fatal("nested runtime artifact change was committed or lost; want it left unstaged in worktree")
	}
}

func TestCheckpointWorktreeSkipsRuntimeOnlyNestedArtifacts(t *testing.T) {
	workDir := t.TempDir()
	mustRunGit(t, workDir, "init")
	mustRunGit(t, workDir, "config", "user.name", "Checkpoint Dog")
	mustRunGit(t, workDir, "config", "user.email", "checkpoint@example.com")

	if err := os.MkdirAll(filepath.Join(workDir, "web", ".beads"), 0o755); err != nil {
		t.Fatalf("setup nested runtime dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "web", ".beads", "redirect"), []byte("before\n"), 0o644); err != nil {
		t.Fatalf("write runtime file: %v", err)
	}
	mustRunGit(t, workDir, "add", "web/.beads/redirect")
	mustRunGit(t, workDir, "commit", "-m", "initial")
	before := mustRunGit(t, workDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(workDir, "web", ".beads", "redirect"), []byte("after\n"), 0o644); err != nil {
		t.Fatalf("modify runtime file: %v", err)
	}

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint for runtime-only changes")
	}
	if after := mustRunGit(t, workDir, "rev-parse", "HEAD"); after != before {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want %s", after, before)
	}
	if got := strings.TrimSpace(mustRunGit(t, workDir, "diff", "--cached", "--name-only")); got != "" {
		t.Fatalf("runtime artifact remained staged: %q", got)
	}
	if got := strings.TrimSpace(mustRunGit(t, workDir, "diff", "--", "web/.beads/redirect")); got == "" {
		t.Fatal("nested runtime artifact change was lost; want it left unstaged in worktree")
	}
}

func mustRunGit(t *testing.T, workDir string, args ...string) string {
	t.Helper()
	out, err := runGitCmd(workDir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// checkpointAlertRecorder stands in for the daemon's checkpointRevertAlert,
// which shells out to `gt escalate`: the escalation must be observable in a
// test, not a real town write (mirrors rigStatusAlerts in rig_status_test.go).
type checkpointAlertRecorder struct {
	calls []checkpointAlertCall
}

type checkpointAlertCall struct {
	key, source, message string
}

func (r *checkpointAlertRecorder) alert(key, source, message string) {
	r.calls = append(r.calls, checkpointAlertCall{key: key, source: source, message: message})
}

// newCheckpointRevertScenario reproduces the shared-worktree-reuse shape from
// gt-2bp8: a bare "origin", a seed checkout that stands in for main, and a
// polecat checkout on its own branch that has already fast-forwarded through
// another polecat's merge — so origin/main and the branch's own ancestry both
// show keep.txt at its post-merge content ("keep\nmain touch\n"). Returns the
// polecat checkout's path; the caller decides what to do to its working tree
// from there.
func newCheckpointRevertScenario(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	mustRunGit(t, "", "init", "--bare", remote)
	// Point the bare repo's HEAD at main explicitly: git init's default branch
	// name is host-configurable, and the clones below check out whatever HEAD
	// names.
	mustRunGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	mustRunGit(t, "", "clone", remote, seed)
	mustRunGit(t, seed, "config", "user.email", "seed@example.com")
	mustRunGit(t, seed, "config", "user.name", "Seed")
	if err := os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	mustRunGit(t, seed, "add", "-A")
	mustRunGit(t, seed, "commit", "-m", "base")
	mustRunGit(t, seed, "push", "origin", "main")

	// The polecat worktree is cut here, at the base commit.
	mustRunGit(t, "", "clone", remote, polecat)
	mustRunGit(t, polecat, "config", "user.email", "polecat@example.com")
	mustRunGit(t, polecat, "config", "user.name", "Polecat")
	mustRunGit(t, polecat, "switch", "-c", "polecat/turquoise/gt-test")

	// A different polecat's work merges into main while this worktree sits at
	// the base commit — the gt-wprt/gt-rv8h merges from the incident report.
	if err := os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("keep\nmain touch\n"), 0o644); err != nil {
		t.Fatalf("advance keep.txt: %v", err)
	}
	mustRunGit(t, seed, "add", "-A")
	mustRunGit(t, seed, "commit", "-m", "merged: other polecat's work")
	mustRunGit(t, seed, "push", "origin", "main")

	// The reused worktree picks up the merge, so its own ancestry already
	// contains it — exactly what a shared-worktree reset leaves behind.
	mustRunGit(t, polecat, "fetch", "origin")
	mustRunGit(t, polecat, "merge", "--ff-only", "origin/main")

	return polecat
}

// TestCheckpointWorktreeRefusesRevertOfMergedWork reproduces gt-2bp8: a
// shared-worktree reset left keep.txt holding stale pre-merge content even
// though this branch's own HEAD (and origin/main) already moved past it, and
// real WIP work sits right alongside the pollution — exactly the mixed shape
// checkpoint_dog must separate. The auto-checkpoint must refuse to commit
// rather than bake the stale content into the branch as a "real" change.
func TestCheckpointWorktreeRefusesRevertOfMergedWork(t *testing.T) {
	polecat := newCheckpointRevertScenario(t)

	// The bug: working-tree pollution reintroduces the PRE-merge content for a
	// file this session never meant to touch.
	if err := os.WriteFile(filepath.Join(polecat, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("pollute keep.txt: %v", err)
	}
	// Real work, staged alongside the pollution — the checkpoint must be able
	// to refuse the revert without the presence of genuine WIP work fooling it
	// into committing anyway.
	if err := os.WriteFile(filepath.Join(polecat, "wip.txt"), []byte("real work in progress\n"), 0o644); err != nil {
		t.Fatalf("write wip.txt: %v", err)
	}

	beforeHead := mustRunGit(t, polecat, "rev-parse", "HEAD")

	alerts := &checkpointAlertRecorder{}
	d := &Daemon{
		logger:                log.New(io.Discard, "", 0),
		checkpointRevertAlert: alerts.alert,
	}
	if d.checkpointWorktree(polecat, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint that reverts already-merged work")
	}

	if afterHead := mustRunGit(t, polecat, "rev-parse", "HEAD"); afterHead != beforeHead {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want unchanged %s", afterHead, beforeHead)
	}

	if len(alerts.calls) != 1 {
		t.Fatalf("expected exactly one revert-guard escalation, got %d: %+v", len(alerts.calls), alerts.calls)
	}
	if !strings.Contains(alerts.calls[0].message, "keep.txt") {
		t.Errorf("escalation message missing the reverted path keep.txt: %s", alerts.calls[0].message)
	}
}

// TestCheckpointWorktreeAllowsLegitimateWorkAgainstMergedTarget is the
// false-positive control for the guard above: the same merged-target shape,
// but the working tree carries only real, non-reverting work. The guard must
// not block an ordinary checkpoint just because origin/main happens to be
// resolvable and ahead of the worktree's starting point.
func TestCheckpointWorktreeAllowsLegitimateWorkAgainstMergedTarget(t *testing.T) {
	polecat := newCheckpointRevertScenario(t)

	if err := os.WriteFile(filepath.Join(polecat, "wip.txt"), []byte("real work in progress\n"), 0o644); err != nil {
		t.Fatalf("write wip.txt: %v", err)
	}

	alerts := &checkpointAlertRecorder{}
	d := &Daemon{
		logger:                log.New(io.Discard, "", 0),
		checkpointRevertAlert: alerts.alert,
	}
	if !d.checkpointWorktree(polecat, "rig", "polecat") {
		t.Fatal("checkpointWorktree refused a checkpoint that does not revert any merged work")
	}
	if len(alerts.calls) != 0 {
		t.Errorf("expected no revert-guard escalation for legitimate work, got %+v", alerts.calls)
	}
	if got := strings.TrimSpace(mustRunGit(t, polecat, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")); got != "wip.txt" {
		t.Errorf("checkpoint commit changed %q, want only wip.txt", got)
	}
}

// TestCheckpointRevertTarget_UsesRigConfigDefaultBranch reproduces the gt-dw43
// mismatch: a rig whose default branch is not "main" was still guarded
// against origin/main, a ref that never even resolves there. The target must
// follow the rig's configured default branch, the same source gt done itself
// reads.
func TestCheckpointRevertTarget_UsesRigConfigDefaultBranch(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	workDir := filepath.Join(dir, "polecat")

	mustRunGit(t, "", "init", "--bare", remote)
	mustRunGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	mustRunGit(t, "", "clone", remote, workDir)
	mustRunGit(t, workDir, "config", "user.email", "polecat@example.com")
	mustRunGit(t, workDir, "config", "user.name", "Polecat")
	if err := os.WriteFile(filepath.Join(workDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base.txt: %v", err)
	}
	mustRunGit(t, workDir, "add", "-A")
	mustRunGit(t, workDir, "commit", "-m", "base")
	mustRunGit(t, workDir, "push", "origin", "HEAD:trunk")

	townRoot := filepath.Join(dir, "town")
	rigPath := filepath.Join(townRoot, "rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"default_branch":"trunk"}`), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	d := &Daemon{
		logger: log.New(io.Discard, "", 0),
		config: &Config{TownRoot: townRoot},
	}
	if got, want := d.checkpointRevertTarget(workDir, "rig"), "origin/trunk"; got != want {
		t.Errorf("checkpointRevertTarget() = %q, want %q", got, want)
	}
}

// TestCheckpointRevertTarget_ForkBackedRigUsesUpstream reproduces the other
// half of gt-dw43: even on a rig whose default branch really is "main",
// origin/main is the wrong ref in a fork-backed rig, where origin is the
// fork and upstream carries the shared history. gt done's own base
// resolution (git.CleanDefaultBranchBaseRef) targets upstream/<default> there
// instead, and the daemon's guard must agree.
func TestCheckpointRevertTarget_ForkBackedRigUsesUpstream(t *testing.T) {
	dir := t.TempDir()
	originRemote := filepath.Join(dir, "origin.git")
	upstreamRemote := filepath.Join(dir, "upstream.git")
	workDir := filepath.Join(dir, "polecat")

	// origin and upstream are distinct bare repos at distinct paths — origin
	// stands in for the fork, upstream for the shared canonical repo. Only a
	// URL difference between them makes ForkBackedRemote true.
	mustRunGit(t, "", "init", "--bare", originRemote)
	mustRunGit(t, "", "init", "--bare", upstreamRemote)

	mustRunGit(t, "", "init", workDir)
	mustRunGit(t, workDir, "config", "user.email", "polecat@example.com")
	mustRunGit(t, workDir, "config", "user.name", "Polecat")
	mustRunGit(t, workDir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base.txt: %v", err)
	}
	mustRunGit(t, workDir, "add", "-A")
	mustRunGit(t, workDir, "commit", "-m", "base")
	mustRunGit(t, workDir, "remote", "add", "origin", originRemote)
	mustRunGit(t, workDir, "remote", "add", "upstream", upstreamRemote)
	mustRunGit(t, workDir, "push", "origin", "HEAD:main")
	mustRunGit(t, workDir, "push", "upstream", "HEAD:main")
	mustRunGit(t, workDir, "fetch", "upstream")

	townRoot := filepath.Join(dir, "town")
	rigPath := filepath.Join(townRoot, "rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"default_branch":"main"}`), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}

	d := &Daemon{
		logger: log.New(io.Discard, "", 0),
		config: &Config{TownRoot: townRoot},
	}
	if got, want := d.checkpointRevertTarget(workDir, "rig"), "upstream/main"; got != want {
		t.Errorf("checkpointRevertTarget() = %q, want %q", got, want)
	}
}

// TestCheckpointRevertTarget_NoRigConfigFallsBackToMain covers the case the
// old hardcoded constant handled correctly: no rig config reachable (nil
// daemon config, as in the other checkpoint_dog tests in this file) still
// falls back to origin/main rather than erroring.
func TestCheckpointRevertTarget_NoRigConfigFallsBackToMain(t *testing.T) {
	polecat := newCheckpointRevertScenario(t)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if got, want := d.checkpointRevertTarget(polecat, "rig"), "origin/main"; got != want {
		t.Errorf("checkpointRevertTarget() = %q, want %q", got, want)
	}
}

// TestCheckpointWorktreeExcludesThrowawayFiles reproduces gt-ozo4: a polecat
// wrote a throwaway diagnostic test file in its worktree to poke at a live
// system, and deleted it minutes later. The checkpoint dog's `git add -A`
// snapshotted it mid-window, and gt done's squash carries HEAD's *tree* into
// the submitted commit — squashing rewrites commit messages, not content — so
// the scratch file would have landed on main. The checkpoint must snapshot the
// real work and leave the throwaway file untracked where the polecat left it.
func TestCheckpointWorktreeExcludesThrowawayFiles(t *testing.T) {
	workDir := t.TempDir()
	mustRunGit(t, workDir, "init")
	mustRunGit(t, workDir, "config", "user.name", "Checkpoint Dog")
	mustRunGit(t, workDir, "config", "user.email", "checkpoint@example.com")

	if err := os.MkdirAll(filepath.Join(workDir, "internal", "util"), 0o755); err != nil {
		t.Fatalf("setup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "internal", "util", "client.go"), []byte("package util\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	mustRunGit(t, workDir, "add", "-A")
	mustRunGit(t, workDir, "commit", "-m", "initial")

	// Real work in progress, alongside the throwaway diagnostic file.
	if err := os.WriteFile(filepath.Join(workDir, "internal", "util", "client.go"), []byte("package util\n\n// real work\n"), 0o644); err != nil {
		t.Fatalf("modify source: %v", err)
	}
	throwaway := filepath.Join(workDir, "internal", "util", "zz_livecheck_test.go")
	if err := os.WriteFile(throwaway, []byte("package util\n"), 0o644); err != nil {
		t.Fatalf("write throwaway: %v", err)
	}

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}

	if got := strings.TrimSpace(mustRunGit(t, workDir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")); got != "internal/util/client.go" {
		t.Fatalf("checkpoint commit changed %q, want only internal/util/client.go", got)
	}
	if tracked := strings.TrimSpace(mustRunGit(t, workDir, "ls-files", "--", "internal/util/zz_livecheck_test.go")); tracked != "" {
		t.Fatalf("throwaway file reached the branch: %q", tracked)
	}
	if _, err := os.Stat(throwaway); err != nil {
		t.Fatalf("throwaway file was removed from the worktree: %v", err)
	}
	if got := strings.TrimSpace(mustRunGit(t, workDir, "status", "--porcelain", "--", "internal/util/zz_livecheck_test.go")); got != "?? internal/util/zz_livecheck_test.go" {
		t.Fatalf("throwaway file status = %q, want it left untracked", got)
	}
}

// TestCheckpointWorktreeSkipsThrowawayOnlyChanges is the boundary for the rule
// above: when a throwaway file is the only thing dirty, the checkpoint has
// nothing to protect and must not create a commit for it.
func TestCheckpointWorktreeSkipsThrowawayOnlyChanges(t *testing.T) {
	workDir := t.TempDir()
	mustRunGit(t, workDir, "init")
	mustRunGit(t, workDir, "config", "user.name", "Checkpoint Dog")
	mustRunGit(t, workDir, "config", "user.email", "checkpoint@example.com")

	if err := os.MkdirAll(filepath.Join(workDir, "internal", "util"), 0o755); err != nil {
		t.Fatalf("setup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "internal", "util", "client.go"), []byte("package util\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	mustRunGit(t, workDir, "add", "-A")
	mustRunGit(t, workDir, "commit", "-m", "initial")
	before := mustRunGit(t, workDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(workDir, "internal", "util", "zz_livecheck_test.go"), []byte("package util\n"), 0o644); err != nil {
		t.Fatalf("write throwaway: %v", err)
	}

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint for a throwaway file")
	}
	if after := mustRunGit(t, workDir, "rev-parse", "HEAD"); after != before {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want %s", after, before)
	}
	if got := strings.TrimSpace(mustRunGit(t, workDir, "status", "--porcelain", "--", "internal/util/zz_livecheck_test.go")); got != "?? internal/util/zz_livecheck_test.go" {
		t.Fatalf("throwaway file status = %q, want it left untracked", got)
	}
}

// TestCheckpointWorktreeCheckpointsTrackedFileMatchingTheRule is the
// false-positive control for the throwaway rule. The rule governs what a
// checkpoint ADDS: a repository that already tracks such a name keeps being
// checkpointed, because a modification to a tracked file is real work whatever
// the file is called.
func TestCheckpointWorktreeCheckpointsTrackedFileMatchingTheRule(t *testing.T) {
	workDir := t.TempDir()
	mustRunGit(t, workDir, "init")
	mustRunGit(t, workDir, "config", "user.name", "Checkpoint Dog")
	mustRunGit(t, workDir, "config", "user.email", "checkpoint@example.com")

	if err := os.WriteFile(filepath.Join(workDir, "zz_fixture_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mustRunGit(t, workDir, "add", "-A")
	mustRunGit(t, workDir, "commit", "-m", "initial")

	if err := os.WriteFile(filepath.Join(workDir, "zz_fixture_test.go"), []byte("package main\n\n// real work\n"), 0o644); err != nil {
		t.Fatalf("modify fixture: %v", err)
	}

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree refused a modification to a tracked file")
	}
	if got := strings.TrimSpace(mustRunGit(t, workDir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")); got != "zz_fixture_test.go" {
		t.Fatalf("checkpoint commit changed %q, want zz_fixture_test.go", got)
	}
}

// TestTriggerCheckpointDog_SkipsWhenNotDue is the regression test for
// gt-ima2/gt-gxpwc applied to checkpoint_dog: with a recent last-run record
// on disk, the trigger must decline to start a cycle rather than firing on
// every tick (or every restart) regardless of the persisted schedule.
func TestTriggerCheckpointDog_SkipsWhenNotDue(t *testing.T) {
	townRoot := t.TempDir()
	if err := savePatrolLastRun(townRoot, "checkpoint_dog", time.Now()); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	var buf strings.Builder
	d := &Daemon{
		logger: log.New(&buf, "", 0),
		config: &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				CheckpointDog: &CheckpointDogConfig{Enabled: true},
			},
		},
	}

	d.triggerCheckpointDog()
	if d.checkpointDogRunning.Load() {
		t.Error("a declined trigger must not set the running guard")
	}
	if !strings.Contains(buf.String(), "not due") {
		t.Errorf("expected a not-due log line, got: %q", buf.String())
	}
}
