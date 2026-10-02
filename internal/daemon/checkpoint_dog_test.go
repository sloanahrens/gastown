package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestCheckpointDogInterval_Default(t *testing.T) {
	t.Parallel()
	interval := checkpointDogInterval(nil)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_NilPatrols(t *testing.T) {
	t.Parallel()
	config := &DaemonPatrolConfig{}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_NilCheckpointDog(t *testing.T) {
	t.Parallel()
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{},
	}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_Configured(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// checkpointClone gives d a gitfake world holding a checkout at a fresh
// directory whose one commit on main holds files (its origin is a bare repo
// beside it), and returns the checkout, the world and the checkout's git.
func checkpointClone(t *testing.T, d *Daemon, files map[string]string) (string, *gitfake.Fake, gitfake.WorkTree) {
	t.Helper()
	f := useGitfake(t, d)
	dir := t.TempDir()
	origin, workDir := filepath.Join(dir, "origin.git"), filepath.Join(dir, "polecat")
	f.InitBare(t, origin)
	f.Commit(t, origin, "main", "initial", files)
	f.Clone(t, origin, workDir)
	return workDir, f, f.Open(workDir).(gitfake.WorkTree)
}

// writeWorkFiles writes files (slash paths to content) into a checkout.
func writeWorkFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func headOf(t *testing.T, g gitfake.WorkTree) string {
	t.Helper()
	head, err := g.(gitfake.Repo).Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev(HEAD): %v", err)
	}
	return head
}

// changedSince lists the files whose content differs between commit before
// and HEAD, sorted.
func changedSince(t *testing.T, g gitfake.WorkTree, before string) []string {
	t.Helper()
	old, err := g.TreeFileBlobs(before)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := g.TreeFileBlobs("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for p, b := range cur {
		if old[p] != b {
			paths = append(paths, p)
		}
	}
	for p := range old {
		if _, ok := cur[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// wantNothingStaged fails when the index differs from HEAD.
func wantNothingStaged(t *testing.T, g gitfake.WorkTree) {
	t.Helper()
	if staged, err := g.StagedChanges(); err != nil || len(staged) != 0 {
		t.Fatalf("staged after the checkpoint = %v, %v; want nothing", staged, err)
	}
}

func worktreeStatus(t *testing.T, g gitfake.WorkTree) *gtgit.GitStatus {
	t.Helper()
	st, err := g.Status()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCheckpointWorktreeExcludesNestedRuntimeArtifacts(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"src/app.go": "package main\n", "web/.beads/redirect": "before\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{"src/app.go": "package main\n// checkpoint me\n", "web/.beads/redirect": "after\n"})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"src/app.go"}) {
		t.Fatalf("checkpoint commit changed %q, want only src/app.go", got)
	}
	wantNothingStaged(t, g)
	if st := worktreeStatus(t, g); !slices.Contains(st.Modified, "web/.beads/redirect") {
		t.Fatalf("status %+v: nested runtime artifact change was committed or lost; want it left unstaged in worktree", st)
	}
}

func TestCheckpointWorktreeSkipsRuntimeOnlyNestedArtifacts(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"web/.beads/redirect": "before\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{"web/.beads/redirect": "after\n"})

	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint for runtime-only changes")
	}
	if after := headOf(t, g); after != before {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want %s", after, before)
	}
	wantNothingStaged(t, g)
	if st := worktreeStatus(t, g); !slices.Contains(st.Modified, "web/.beads/redirect") {
		t.Fatal("nested runtime artifact change was lost; want it left unstaged in worktree")
	}
}

// A worktree with nothing to checkpoint makes no commit.
func TestCheckpointWorktreeSkipsACleanWorktree(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"a.go": "package a\n"})
	before := headOf(t, g)
	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree checkpointed a clean worktree")
	}
	if after := headOf(t, g); after != before {
		t.Fatalf("HEAD moved to %s", after)
	}
}

// A deleted tracked file is never committed as a deletion (gt-pvx): the
// checkpoint keeps the file and records the rest of the work.
func TestCheckpointWorktreeNeverCommitsADeletion(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"keep.go": "package a\n", "work.go": "package a\n"})
	before := headOf(t, g)
	if err := os.Remove(filepath.Join(workDir, "keep.go")); err != nil {
		t.Fatal(err)
	}
	writeWorkFiles(t, workDir, map[string]string{"work.go": "package a\n// wip\n"})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not checkpoint the real work")
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"work.go"}) {
		t.Fatalf("checkpoint commit changed %q, want only work.go (never the deletion of keep.go)", got)
	}
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
// gt-2bp8: an "origin" standing in for main, and a polecat checkout on its own
// branch that has already merged another polecat's work from main — so
// origin/main and the branch's own ancestry both show keep.txt at its
// post-merge content ("keep\nmain touch\n"). It wires d's git to the world
// and returns the polecat checkout's path and git; the caller decides what to
// do to its working tree from there.
func newCheckpointRevertScenario(t *testing.T, d *Daemon) (string, gitfake.WorkTree) {
	t.Helper()
	f := useGitfake(t, d)
	dir := t.TempDir()
	origin, polecat := filepath.Join(dir, "origin.git"), filepath.Join(dir, "polecat")
	const branch = "polecat/turquoise/gt-test"

	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "base", map[string]string{"keep.txt": "keep\n"})
	// The polecat worktree is cut here, at the base commit, on its own branch.
	f.SetRef(t, origin, "refs/heads/"+branch, base)
	if err := f.Open(dir).CloneBranch(origin, polecat, branch); err != nil {
		t.Fatal(err)
	}

	// A different polecat's work merges into main while this worktree sits at
	// the base commit — the gt-wprt/gt-rv8h merges from the incident report.
	f.Commit(t, origin, "main", "merged: other polecat's work", map[string]string{"keep.txt": "keep\nmain touch\n"})

	// The reused worktree picks up the merge, so its own ancestry already
	// contains it — exactly what a shared-worktree reset leaves behind.
	g := f.Open(polecat)
	if err := g.FetchRefspecWithTimeout("origin", "+refs/heads/main:refs/remotes/origin/main", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := g.MergeNoFF("origin/main", "merge main"); err != nil {
		t.Fatal(err)
	}
	return polecat, g.(gitfake.WorkTree)
}

// TestCheckpointWorktreeRefusesRevertOfMergedWork reproduces gt-2bp8: a
// shared-worktree reset left keep.txt holding stale pre-merge content even
// though this branch's own HEAD (and origin/main) already moved past it, and
// real WIP work sits right alongside the pollution — exactly the mixed shape
// checkpoint_dog must separate. The auto-checkpoint must refuse to commit
// rather than bake the stale content into the branch as a "real" change.
func TestCheckpointWorktreeRefusesRevertOfMergedWork(t *testing.T) {
	t.Parallel()
	alerts := &checkpointAlertRecorder{}
	d := &Daemon{
		logger:                log.New(io.Discard, "", 0),
		checkpointRevertAlert: alerts.alert,
	}
	polecat, g := newCheckpointRevertScenario(t, d)

	// The bug: working-tree pollution reintroduces the PRE-merge content for a
	// file this session never meant to touch, beside real work — the
	// checkpoint must be able to refuse the revert without the presence of
	// genuine WIP work fooling it into committing anyway.
	writeWorkFiles(t, polecat, map[string]string{"keep.txt": "keep\n", "wip.txt": "real work in progress\n"})
	beforeHead := headOf(t, g)

	if d.checkpointWorktree(polecat, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint that reverts already-merged work")
	}
	if afterHead := headOf(t, g); afterHead != beforeHead {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want unchanged %s", afterHead, beforeHead)
	}
	if len(alerts.calls) != 1 {
		t.Fatalf("expected exactly one revert-guard escalation, got %d: %+v", len(alerts.calls), alerts.calls)
	}
	if !strings.Contains(alerts.calls[0].message, "keep.txt") {
		t.Errorf("escalation message missing the reverted path keep.txt: %s", alerts.calls[0].message)
	}
	if alerts.calls[0].key != "checkpoint_dog:revert-guard:rig/polecat" {
		t.Errorf("escalation key = %q", alerts.calls[0].key)
	}
}

// TestCheckpointWorktreeAllowsLegitimateWorkAgainstMergedTarget is the
// false-positive control for the guard above: the same merged-target shape,
// but the working tree carries only real, non-reverting work. The guard must
// not block an ordinary checkpoint just because origin/main happens to be
// resolvable and ahead of the worktree's starting point.
func TestCheckpointWorktreeAllowsLegitimateWorkAgainstMergedTarget(t *testing.T) {
	t.Parallel()
	alerts := &checkpointAlertRecorder{}
	d := &Daemon{
		logger:                log.New(io.Discard, "", 0),
		checkpointRevertAlert: alerts.alert,
	}
	polecat, g := newCheckpointRevertScenario(t, d)
	before := headOf(t, g)
	writeWorkFiles(t, polecat, map[string]string{"wip.txt": "real work in progress\n"})

	if !d.checkpointWorktree(polecat, "rig", "polecat") {
		t.Fatal("checkpointWorktree refused a checkpoint that does not revert any merged work")
	}
	if len(alerts.calls) != 0 {
		t.Errorf("expected no revert-guard escalation for legitimate work, got %+v", alerts.calls)
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"wip.txt"}) {
		t.Errorf("checkpoint commit changed %q, want only wip.txt", got)
	}
}

// TestCheckpointRevertTarget_UsesRigConfigDefaultBranch reproduces the gt-dw43
// mismatch: a rig whose default branch is not "main" was still guarded
// against origin/main, a ref that never even resolves there. The target must
// follow the rig's configured default branch, the same source gt done itself
// reads.
func TestCheckpointRevertTarget_UsesRigConfigDefaultBranch(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig path: %v", err)
	}
	// Identity keys too: one strict loader owns rig config.json (gt-y3pgh.2.5),
	// and a file without them does not decode, so the branch read would
	// silently fall back to "main".
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"type":"rig","version":1,"name":"rig","default_branch":"trunk"}`), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: townRoot}}
	workDir, _, _ := checkpointClone(t, d, map[string]string{"base.txt": "base\n"})

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
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"type":"rig","version":1,"name":"rig","default_branch":"main"}`), 0o644); err != nil {
		t.Fatalf("write rig config.json: %v", err)
	}
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: townRoot}}
	workDir, f, _ := checkpointClone(t, d, map[string]string{"base.txt": "base\n"})
	// origin stands in for the fork, upstream for the shared canonical repo at
	// a distinct path: only a URL difference makes the rig fork-backed.
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	f.InitBare(t, upstream)
	if err := f.Open(workDir).AddUpstreamRemote(upstream); err != nil {
		t.Fatal(err)
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
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	polecat, _ := newCheckpointRevertScenario(t, d)

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
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"internal/util/client.go": "package util\n"})
	before := headOf(t, g)

	// Real work in progress, alongside the throwaway diagnostic file.
	writeWorkFiles(t, workDir, map[string]string{
		"internal/util/client.go":            "package util\n\n// real work\n",
		"internal/util/zz_livecheck_test.go": "package util\n",
	})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"internal/util/client.go"}) {
		t.Fatalf("checkpoint commit changed %q, want only internal/util/client.go", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "internal", "util", "zz_livecheck_test.go")); err != nil {
		t.Fatalf("throwaway file was removed from the worktree: %v", err)
	}
	if st := worktreeStatus(t, g); !slices.Equal(st.Untracked, []string{"internal/util/zz_livecheck_test.go"}) {
		t.Fatalf("untracked = %q, want the throwaway file left untracked", st.Untracked)
	}
}

// TestCheckpointWorktreeSkipsThrowawayOnlyChanges is the boundary for the rule
// above: when a throwaway file is the only thing dirty, the checkpoint has
// nothing to protect and must not create a commit for it.
func TestCheckpointWorktreeSkipsThrowawayOnlyChanges(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"internal/util/client.go": "package util\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{"internal/util/zz_livecheck_test.go": "package util\n"})

	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint for a throwaway file")
	}
	if after := headOf(t, g); after != before {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want %s", after, before)
	}
	if st := worktreeStatus(t, g); !slices.Equal(st.Untracked, []string{"internal/util/zz_livecheck_test.go"}) {
		t.Fatalf("untracked = %q, want the throwaway file left untracked", st.Untracked)
	}
}

// TestCheckpointWorktreeExcludesThrowawayThatLooksLikeARename reproduces the
// fail-open in the throwaway filter: `git add -A` stages a tracked file that was
// renamed to a throwaway name as a delete plus an add, and git's rename
// detection reports that pair as a single R entry, which a filter on additions
// does not list. The scratch name reached the checkpoint commit and, through
// gt done's tree squash, the target.
func TestCheckpointWorktreeExcludesThrowawayThatLooksLikeARename(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{
		"helper.go": "package main\n\nfunc helper() {}\n",
		"client.go": "package main\n",
	})
	before := headOf(t, g)

	// Real work, plus a tracked file moved to a scratch name.
	writeWorkFiles(t, workDir, map[string]string{"client.go": "package main\n\n// real work\n"})
	if err := os.Rename(filepath.Join(workDir, "helper.go"), filepath.Join(workDir, "helper_tmp.go")); err != nil {
		t.Fatalf("rename to throwaway: %v", err)
	}

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"client.go"}) {
		t.Fatalf("checkpoint commit changed %q, want only client.go", got)
	}
	blobs, err := g.TreeFileBlobs("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, tracked := blobs["helper_tmp.go"]; tracked {
		t.Fatal("throwaway file reached the branch")
	}
}

// TestCheckpointWorktreeCheckpointsTrackedFileMatchingTheRule is the
// false-positive control for the throwaway rule. The rule governs what a
// checkpoint ADDS: a repository that already tracks such a name keeps being
// checkpointed, because a modification to a tracked file is real work whatever
// the file is called.
func TestCheckpointWorktreeCheckpointsTrackedFileMatchingTheRule(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"zz_fixture_test.go": "package main\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{"zz_fixture_test.go": "package main\n\n// real work\n"})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree refused a modification to a tracked file")
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"zz_fixture_test.go"}) {
		t.Fatalf("checkpoint commit changed %q, want zz_fixture_test.go", got)
	}
}

// TestTriggerCheckpointDog_SkipsWhenNotDue is the regression test for
// gt-ima2/gt-gxpwc applied to checkpoint_dog: with a recent last-run record
// on disk, the trigger must decline to start a cycle rather than firing on
// every tick (or every restart) regardless of the persisted schedule.
func TestTriggerCheckpointDog_SkipsWhenNotDue(t *testing.T) {
	t.Parallel()
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
