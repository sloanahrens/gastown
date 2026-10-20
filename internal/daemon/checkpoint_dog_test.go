package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/revertwaiver"
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
	got := resolvePolecatWorktree(polecatsDir, polecat, rig)
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
	got := resolvePolecatWorktree(polecatsDir, polecat, rig)
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
	// resolvePolecatWorktree must NOT return a path that lets git walk
	// to this — it should return "" so the caller skips entirely.
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("setup parent .git: %v", err)
	}
	got := resolvePolecatWorktree(polecatsDir, polecat, rig)
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
	got := resolvePolecatWorktree(polecatsDir, polecat, rig)
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

// A fixture under a coverage/ directory that holds tracked, unchanged source is
// work: the staged list names only the fixture, so the runtime reset must learn
// of the source from the checkout, while generated output elsewhere still
// stays out (gt-za3u1).
func TestCheckpointWorktreeKeepsFixtureBesideTrackedSource(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"internal/coverage/collector.go": "package coverage\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{
		"internal/coverage/testdata/report.golden": "golden\n",
		"web/coverage/lcov.info":                   "TN:\n",
	})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"internal/coverage/testdata/report.golden"}) {
		t.Fatalf("checkpoint commit changed %q, want only the fixture", got)
	}
	wantNothingStaged(t, g)
	if st := worktreeStatus(t, g); !slices.Contains(st.Untracked, "web/coverage/lcov.info") {
		t.Fatalf("status %+v: generated coverage output was committed or lost; want it left untracked", st)
	}
}

// A new source file and a new fixture beside it, with nothing committed: HEAD
// vouches for neither, but a golden file is hand-written work, so the
// checkpoint commits both while the generated output beside them stays out
// (gt-v90u4).
func TestCheckpointWorktreeKeepsNewFixtureBesideNewSource(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"README.md": "readme\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{
		"internal/coverage/collector.go":           "package coverage\n",
		"internal/coverage/testdata/report.golden": "golden\n",
		"internal/coverage/coverage.out":           "mode: set\n",
		"internal/coverage/lcov-report/index.html": "<html>\n",
	})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}
	want := []string{"internal/coverage/collector.go", "internal/coverage/testdata/report.golden"}
	if got := changedSince(t, g, before); !slices.Equal(got, want) {
		t.Fatalf("checkpoint commit changed %q, want %q", got, want)
	}
	wantNothingStaged(t, g)
	st := worktreeStatus(t, g)
	for _, generated := range []string{"internal/coverage/coverage.out", "internal/coverage/lcov-report/index.html"} {
		if !slices.Contains(st.Untracked, generated) {
			t.Errorf("status %+v: %s was committed or lost; want it left untracked", st, generated)
		}
	}
}

// An edited source file under a coverage/ directory does not make the output
// written beside it work: the checkpoint commits the edit and a modified
// fixture HEAD tracks, and leaves the generated files out (gt-qdvp2).
func TestCheckpointWorktreeResetsOutputBesideEditedSourceButKeepsTrackedFixture(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{
		"pkg/coverage/collector.go":           "package coverage\n",
		"pkg/coverage/testdata/report.golden": "old\n",
	})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{
		"pkg/coverage/collector.go":           "package coverage\n// edited\n",
		"pkg/coverage/testdata/report.golden": "new\n",
		"pkg/coverage/lcov-report/index.html": "<html>\n",
		"pkg/coverage/coverage.out":           "mode: set\n",
	})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}
	want := []string{"pkg/coverage/collector.go", "pkg/coverage/testdata/report.golden"}
	if got := changedSince(t, g, before); !slices.Equal(got, want) {
		t.Fatalf("checkpoint commit changed %q, want %q", got, want)
	}
	wantNothingStaged(t, g)
	st := worktreeStatus(t, g)
	for _, generated := range []string{"pkg/coverage/lcov-report/index.html", "pkg/coverage/coverage.out"} {
		if !slices.Contains(st.Untracked, generated) {
			t.Errorf("status %+v: %s was committed or lost; want it left untracked", st, generated)
		}
	}
}

// A committed report under coverage/, with no source file beside it, is no
// more an artifact directory than one holding source: the checkpoint commits
// the polecat's edit to the report, while generated output written under the
// same directory stays out of the commit (gt-42qp3).
func TestCheckpointWorktreeKeepsEditToCommittedReportUnderCoverage(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"coverage/report.html": "<html>before</html>\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{
		"coverage/report.html":  "<html>after</html>\n",
		"coverage/coverage.out": "mode: set\n",
	})

	if !d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree did not create a checkpoint commit")
	}
	if got := changedSince(t, g, before); !slices.Equal(got, []string{"coverage/report.html"}) {
		t.Fatalf("checkpoint commit changed %q, want only the committed report", got)
	}
	wantNothingStaged(t, g)
	if st := worktreeStatus(t, g); !slices.Contains(st.Untracked, "coverage/coverage.out") {
		t.Fatalf("status %+v: generated output was committed or lost; want it left untracked", st)
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

// TestCheckpointWorktreeSkipsAMergeInProgress reproduces gt-kbp1t: a polecat
// worktree stopped on a merge carries MERGE_HEAD in its git directory, and a
// `git add -A` plus commit there would record the conflict markers — literal
// `<<<<<<<` lines — as work on the polecat's branch.
func TestCheckpointWorktreeSkipsAMergeInProgress(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0)}
	workDir, _, g := checkpointClone(t, d, map[string]string{"a.go": "package a\n"})
	before := headOf(t, g)
	writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n<<<<<<< HEAD\n"})
	if err := os.WriteFile(filepath.Join(workDir, ".git", "MERGE_HEAD"), []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
		t.Fatalf("write MERGE_HEAD: %v", err)
	}

	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree committed a worktree with a merge in progress")
	}
	if after := headOf(t, g); after != before {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want unchanged %s", after, before)
	}
	wantNothingStaged(t, g)
	if !strings.Contains(buf.String(), "skipping rig/polecat: merge in progress") {
		t.Errorf("log = %q; want a skip line naming the merge", buf.String())
	}
}

// TestCheckpointWorktreeSkipsARebaseInProgress covers the rebase half of
// gt-kbp1t. Both marker directories git leaves for a rebase — rebase-merge for
// a merge-backed rebase, rebase-apply for an apply-backed one — must stop the
// checkpoint, since committing a half-replayed rebase lands markers or
// half-applied history on the branch.
func TestCheckpointWorktreeSkipsARebaseInProgress(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{"rebase-merge", "rebase-apply"} {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			var buf strings.Builder
			d := &Daemon{logger: log.New(&buf, "", 0)}
			workDir, _, g := checkpointClone(t, d, map[string]string{"a.go": "package a\n"})
			before := headOf(t, g)
			writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n// half-replayed\n"})
			if err := os.MkdirAll(filepath.Join(workDir, ".git", marker), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", marker, err)
			}

			if d.checkpointWorktree(workDir, "rig", "polecat") {
				t.Fatal("checkpointWorktree committed a worktree mid-rebase")
			}
			if after := headOf(t, g); after != before {
				t.Fatalf("checkpointWorktree advanced HEAD to %s, want unchanged %s", after, before)
			}
			wantNothingStaged(t, g)
			if !strings.Contains(buf.String(), "skipping rig/polecat: rebase in progress") {
				t.Errorf("log = %q; want a skip line naming the rebase", buf.String())
			}
		})
	}
}

// TestCheckpointWorktreeSkipsUnmergedIndexEntries covers the third gt-kbp1t
// state: an index holding unmerged entries. The merge here leaves no
// MERGE_HEAD file (the fake models the merge state only), so the skip can only
// come from the unmerged-path check — the same set `git diff --name-only
// --diff-filter=U` reports.
func TestCheckpointWorktreeSkipsUnmergedIndexEntries(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0)}
	f := useGitfake(t, d)
	dir := t.TempDir()
	origin, workDir := filepath.Join(dir, "origin.git"), filepath.Join(dir, "polecat")
	const branch = "polecat/agate/gt-kbp1t"
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "base", map[string]string{"a.txt": "base\n"})
	f.SetRef(t, origin, "refs/heads/"+branch, base)
	if err := f.Open(dir).CloneBranch(origin, workDir, branch); err != nil {
		t.Fatal(err)
	}
	g := f.Open(workDir)
	// Both sides change a.txt differently, so the merge stops on a conflict.
	f.Commit(t, workDir, branch, "ours", map[string]string{"a.txt": "ours\n"})
	f.Commit(t, origin, "main", "theirs", map[string]string{"a.txt": "theirs\n"})
	if err := g.FetchRefspecWithTimeout("origin", "+refs/heads/main:refs/remotes/origin/main", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := g.MergeNoFF("origin/main", "merge main"); err == nil {
		t.Fatal("a conflicting merge succeeded")
	}
	before := headOf(t, g.(gitfake.WorkTree))

	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree committed a worktree with unmerged index entries")
	}
	if after := headOf(t, g.(gitfake.WorkTree)); after != before {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want unchanged %s", after, before)
	}
	wantNothingStaged(t, g.(gitfake.WorkTree))
	if !strings.Contains(buf.String(), "skipping rig/polecat: unmerged paths: a.txt") {
		t.Errorf("log = %q; want a skip line naming the unmerged path", buf.String())
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

// unstagingFailsGit wraps a worktree's git so every ResetFiles call fails, the
// way a transient git failure does mid-checkpoint.
type unstagingFailsGit struct {
	daemonGit
}

func (unstagingFailsGit) ResetFiles(pathspecs ...string) error {
	return errors.New("git reset failed (transient)")
}

// A failure to unstage a deletion must abort the checkpoint, not fall through
// to the commit: the commit at the end of the run records whatever is still
// staged, so a deletion left staged by a failed reset would land on the
// polecat's branch (gt-2czgm).
func TestCheckpointWorktreeRefusesWhenUnstagingADeletionFails(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	workDir, f, g := checkpointClone(t, d, map[string]string{"keep.go": "package a\n", "work.go": "package a\n"})
	before := headOf(t, g)
	if err := os.Remove(filepath.Join(workDir, "keep.go")); err != nil {
		t.Fatal(err)
	}
	writeWorkFiles(t, workDir, map[string]string{"work.go": "package a\n// wip\n"})

	d.openGitFn = func(dir string) daemonGit {
		return unstagingFailsGit{daemonGit: f.Open(dir).(daemonGit)}
	}

	if d.checkpointWorktree(workDir, "rig", "polecat") {
		t.Fatal("checkpointWorktree checkpointed although it could not unstage the deletion")
	}
	if after := headOf(t, g); after != before {
		t.Fatalf("checkpoint advanced HEAD to %s, want it unchanged at %s", after, before)
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
	return newCheckpointRevertScenarioOn(t, d, "polecat/turquoise/gt-test")
}

// newCheckpointRevertScenarioOn is newCheckpointRevertScenario with the
// worktree's branch named, so a test can pin the bead the branch carries.
func newCheckpointRevertScenarioOn(t *testing.T, d *Daemon, branch string) (string, gitfake.WorkTree) {
	t.Helper()
	return newCheckpointRevertScenarioFiles(t, d, branch)
}

// newCheckpointRevertScenarioFiles is newCheckpointRevertScenarioOn with more
// files, each merged in a commit of its own: an extra one is "<name>\n" at the
// base and "<name>\ntouch <name>\n" once merged, so a test can stage a revert
// of one merged commit, of several, and move between them. The touched line is
// unique to its file: a line that survives elsewhere reads as a relocation,
// not a revert.
func newCheckpointRevertScenarioFiles(t *testing.T, d *Daemon, branch string, extra ...string) (string, gitfake.WorkTree) {
	t.Helper()
	baseFiles := map[string]string{"keep.txt": "keep\n"}
	for _, name := range extra {
		baseFiles[name] = name + "\n"
	}
	f := useGitfake(t, d)
	dir := t.TempDir()
	origin, polecat := filepath.Join(dir, "origin.git"), filepath.Join(dir, "polecat")

	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "base", baseFiles)
	// The polecat worktree is cut here, at the base commit, on its own branch.
	f.SetRef(t, origin, "refs/heads/"+branch, base)
	if err := f.Open(dir).CloneBranch(origin, polecat, branch); err != nil {
		t.Fatal(err)
	}

	// A different polecat's work merges into main while this worktree sits at
	// the base commit — the gt-wprt/gt-rv8h merges from the incident report.
	f.Commit(t, origin, "main", "merged: other polecat's work", map[string]string{"keep.txt": "keep\nmain touch\n"})
	for _, name := range extra {
		f.Commit(t, origin, "main", "merged: "+name, map[string]string{name: name + "\ntouch " + name + "\n"})
	}

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

// A refusal ends the checkpoint with the files as the polecat left them and
// the index as HEAD has it: the content the revert guard refused must not wait
// in the index for the polecat's next `git commit` (gt-u3gph).
func TestCheckpointRefusalLeavesIndexClean(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		logger:                log.New(io.Discard, "", 0),
		checkpointRevertAlert: (&checkpointAlertRecorder{}).alert,
	}
	polecat, g := newCheckpointRevertScenario(t, d)
	writeWorkFiles(t, polecat, map[string]string{"keep.txt": "keep\n", "wip.txt": "real work in progress\n"})

	if d.checkpointWorktree(polecat, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint that reverts already-merged work")
	}

	staged, err := g.StagedChanges()
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Errorf("refused checkpoint left %v staged for the polecat's next commit", staged)
	}
	for name, want := range map[string]string{"keep.txt": "keep\n", "wip.txt": "real work in progress\n"} {
		got, err := os.ReadFile(filepath.Join(polecat, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want the polecat's content %q untouched", name, got, err, want)
		}
	}
}

// A refusal unstages what the checkpoint staged, not what the polecat had
// already staged itself: its own `git add` stays in the index for its next
// commit.
func TestCheckpointRefusalKeepsThePolecatsOwnStaging(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		logger:                log.New(io.Discard, "", 0),
		checkpointRevertAlert: (&checkpointAlertRecorder{}).alert,
	}
	polecat, g := newCheckpointRevertScenario(t, d)
	writeWorkFiles(t, polecat, map[string]string{"keep.txt": "keep\n", "own.txt": "staged by the polecat\n", "wip.txt": "unstaged work\n"})
	if err := g.Add("own.txt"); err != nil {
		t.Fatal(err)
	}

	if d.checkpointWorktree(polecat, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint that reverts already-merged work")
	}

	staged, err := g.StagedChanges()
	if err != nil {
		t.Fatal(err)
	}
	if got := stagedPaths(staged, 0); !slices.Equal(got, []string{"own.txt"}) {
		t.Errorf("staged after the refusal = %v, want only the polecat's own.txt", got)
	}
}

// TestCheckpointWorktreeDeletesBySpec covers the revert guard against a seat
// whose hooked bead may direct the deletion (gt-p8k4i): the staged tree reverts
// keep.txt, and only a deletes-by-spec bead whose description names that path
// lets the checkpoint through, the same waiver gt done applies.
func TestCheckpointWorktreeDeletesBySpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		labels      []string
		description string
		status      string
		wantCommit  bool
	}{
		{"label and named path", []string{"deletes-by-spec"}, "Delete keep.txt.", "hooked", true},
		{"in_progress bead", []string{"deletes-by-spec"}, "Delete keep.txt.", "in_progress", true},
		{"label without the path named", []string{"deletes-by-spec"}, "Delete the old files.", "hooked", false},
		{"path named without the label", nil, "Delete keep.txt.", "hooked", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alerts := &checkpointAlertRecorder{}
			bd := newWorkBD(t)
			bd.db.Seed(beads.Issue{ID: "gt-del", Status: tt.status, Assignee: "rig/polecats/polecat",
				Labels: tt.labels, Description: tt.description})
			d := &Daemon{
				logger:                log.New(io.Discard, "", 0),
				config:                &Config{TownRoot: t.TempDir()},
				openWorkBeads:         bd.open,
				checkpointRevertAlert: alerts.alert,
			}
			polecat, g := newCheckpointRevertScenario(t, d)
			writeWorkFiles(t, polecat, map[string]string{"keep.txt": "keep\n", "wip.txt": "real work in progress\n"})
			before := headOf(t, g)

			created := d.checkpointWorktree(polecat, "rig", "polecat")
			if created != tt.wantCommit {
				t.Fatalf("checkpointWorktree = %v, want %v", created, tt.wantCommit)
			}
			if tt.wantCommit {
				if got := changedSince(t, g, before); !slices.Equal(got, []string{"keep.txt", "wip.txt"}) {
					t.Errorf("checkpoint commit changed %q, want keep.txt and wip.txt", got)
				}
				if len(alerts.calls) != 0 {
					t.Errorf("a directed deletion raised an escalation: %+v", alerts.calls)
				}
				return
			}
			if after := headOf(t, g); after != before {
				t.Errorf("refused checkpoint advanced HEAD to %s, want %s", after, before)
			}
			if len(alerts.calls) != 1 {
				t.Errorf("want exactly one escalation, got %+v", alerts.calls)
			}
		})
	}
}

// secondWriteTreeFails is a daemonGit whose WriteTree answers once and then
// fails, the shape of an index that cannot be re-read after the waiver's bd
// reads. The count is shared across the handles the daemon opens.
type secondWriteTreeFails struct {
	daemonGit
	calls *int
}

func (g secondWriteTreeFails) WriteTree() (string, error) {
	*g.calls++
	if *g.calls > 1 {
		return "", errors.New("index.lock held")
	}
	return g.daemonGit.WriteTree()
}

// TestCheckpointRevertGuardJudgesTheWaiverAgainstTheTreeAfterTheBeadRead covers
// gt-0h4i0: the deletes-by-spec bead is read with bd while the polecat goes on
// working, so the staged set can move under the read. The waiver is judged
// against the tree staged after it, the refusal names that tree's paths, and a
// re-read that fails refuses rather than waives.
func TestCheckpointRevertGuardJudgesTheWaiverAgainstTheTreeAfterTheBeadRead(t *testing.T) {
	t.Parallel()
	const merged = "keep\nmain touch\n"
	tests := []struct {
		name string
		// staged are the files reverted when the guard starts.
		staged []string
		// duringRead runs when the guard opens the bead store.
		duringRead func(t *testing.T, dir string, g gitfake.WorkTree)
		// failReread makes the second write-tree fail.
		failReread  bool
		wantBlocked bool
		wantReason  []string
	}{
		{
			name:   "the tree stops reverting during the read",
			staged: []string{"keep.txt"},
			duringRead: func(t *testing.T, dir string, g gitfake.WorkTree) {
				writeWorkFiles(t, dir, map[string]string{"keep.txt": merged})
				if err := g.Add("keep.txt"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "a path the bead does not name leaves the tree during the read",
			staged: []string{"keep.txt", "other.txt"},
			duringRead: func(t *testing.T, dir string, g gitfake.WorkTree) {
				writeWorkFiles(t, dir, map[string]string{"other.txt": "other.txt\ntouch other.txt\n"})
				if err := g.Add("other.txt"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "a path the bead does not name joins the tree during the read",
			staged: []string{"keep.txt"},
			duringRead: func(t *testing.T, dir string, g gitfake.WorkTree) {
				writeWorkFiles(t, dir, map[string]string{"other.txt": "other.txt\n"})
				if err := g.Add("other.txt"); err != nil {
					t.Fatal(err)
				}
			},
			wantBlocked: true,
			wantReason:  []string{"keep.txt", "other.txt"},
		},
		{
			name:        "the re-read fails",
			staged:      []string{"keep.txt"},
			failReread:  true,
			wantBlocked: true,
			wantReason:  []string{"could not re-inspect the pending checkpoint", "index.lock held"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alerts := &checkpointAlertRecorder{}
			bd := newWorkBD(t)
			bd.db.Seed(beads.Issue{ID: "gt-del", Status: "hooked", Assignee: "rig/polecats/polecat",
				Labels: []string{"deletes-by-spec"}, Description: "Delete keep.txt."})
			d := &Daemon{
				logger:                log.New(io.Discard, "", 0),
				config:                &Config{TownRoot: t.TempDir()},
				checkpointRevertAlert: alerts.alert,
			}
			polecat, g := newCheckpointRevertScenarioFiles(t, d, "polecat/turquoise/gt-test", "other.txt")
			if tt.failReread {
				open, calls := d.openGitFn, new(int)
				d.openGitFn = func(dir string) daemonGit { return secondWriteTreeFails{open(dir), calls} }
			}
			// The bead store opens once, and that is the moment the polecat's
			// own work moves the index.
			var once sync.Once
			d.openWorkBeads = func(env []string, timeout time.Duration) workBeadReader {
				once.Do(func() {
					if tt.duringRead != nil {
						tt.duringRead(t, polecat, g)
					}
				})
				return bd.open(env, timeout)
			}
			for _, name := range tt.staged {
				content := name + "\n"
				if name == "keep.txt" {
					content = "keep\n"
				}
				writeWorkFiles(t, polecat, map[string]string{name: content})
				if err := g.Add(name); err != nil {
					t.Fatal(err)
				}
			}

			blocked, reason := d.checkpointRevertGuard(polecat, "rig", "polecat")
			if blocked != tt.wantBlocked {
				t.Fatalf("checkpointRevertGuard blocked = %v (%q), want %v", blocked, reason, tt.wantBlocked)
			}
			for _, want := range tt.wantReason {
				if !strings.Contains(reason, want) {
					t.Errorf("reason = %q, want it to name %q", reason, want)
				}
			}
			if len(alerts.calls) > 1 || (len(alerts.calls) == 1 && !blocked) {
				t.Errorf("escalations = %+v, want one only for a revert that stands", alerts.calls)
			}
		})
	}
}

// TestCheckpointWaiverBeadFailsClosed covers every lookup the deletes-by-spec
// waiver makes: a missing config, an unreadable work-bead store and a bead that
// cannot be read all name no bead, so no waiver can be read from them and the
// refusal and its escalation stand (gt-rp83g).
func TestCheckpointWaiverBeadFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  func(*testing.T) *Config
		seed    bool
		breakBD func(*workBD)
		want    bool
	}{
		{"no daemon config", func(*testing.T) *Config { return nil }, true, nil, false},
		{"config without a town root", func(*testing.T) *Config { return &Config{} }, true, nil, false},
		{"work-bead list fails", townConfig, true, func(b *workBD) { b.listErr = errors.New("bd down") }, false},
		{"no assigned work", townConfig, false, nil, false},
		{"bead read fails", townConfig, true, func(b *workBD) { b.showErr = errors.New("bd down") }, false},
		{"label and path named", townConfig, true, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := newWorkBD(t)
			if tt.breakBD != nil {
				tt.breakBD(bd)
			}
			if tt.seed {
				bd.db.Seed(beads.Issue{ID: "gt-del", Status: "hooked", Assignee: "rig/polecats/polecat",
					Labels: []string{revertwaiver.Label}, Description: "Delete keep.txt."})
			}
			d := &Daemon{
				logger:        log.New(io.Discard, "", 0),
				config:        tt.config(t),
				openWorkBeads: bd.open,
			}
			useGitfake(t, d)

			issue := d.checkpointWaiverBead(t.TempDir(), "rig", "polecat")
			if (issue != nil) != tt.want {
				t.Fatalf("checkpointWaiverBead = %+v, want a bead: %v", issue, tt.want)
			}
			if tt.want && issue.ID != "gt-del" {
				t.Errorf("waiver bead = %q, want gt-del", issue.ID)
			}
		})
	}
}

// TestCheckpointedWorkBeadCustomBranchTemplateWithTwoActiveAssignmentsNamesNone
// covers gt-0h4i0: a custom polecat_branch_template gives a branch
// ParseBranchName does not understand, so the waiver falls back to the seat's
// active assignments. A seat holding two of them cannot say which bead directs
// the deletions, and naming either one would let a checkpoint through on the
// wrong bead's authority.
func TestCheckpointedWorkBeadCustomBranchTemplateWithTwoActiveAssignmentsNamesNone(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{ID: "gt-a", Status: "hooked", Assignee: "rig/polecats/polecat"})
	bd.db.Seed(beads.Issue{ID: "gt-b", Status: "in_progress", Assignee: "rig/polecats/polecat"})
	d := &Daemon{
		logger:        log.New(io.Discard, "", 0),
		config:        townConfig(t),
		openWorkBeads: bd.open,
	}
	// A custom template's branch: no polecat/ prefix, so the branch names no
	// issue this package can read.
	polecat, _ := newCheckpointRevertScenarioOn(t, d, "feature/gt-custom")

	got, err := d.checkpointedWorkBead(polecat, "rig", "polecat")
	if err != nil {
		t.Fatalf("checkpointedWorkBead: %v", err)
	}
	if got != "" {
		t.Errorf("checkpointedWorkBead = %q, want no bead: two active assignments and a branch the template does not parse", got)
	}
}

// townConfig is a daemon config over a fresh temp town root.
func townConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{TownRoot: t.TempDir()}
}

// TestCheckpointDeletesBySpecReadsTheBeadTheBranchCarries covers the lookup's
// source: the waiver is read from the issue the checkpointed branch names, not
// from the first active bead the seat also holds. A stale assignment labeled
// deletes-by-spec must not waive a deletion the branch's own bead did not
// direct (gt-rp83g).
func TestCheckpointDeletesBySpecReadsTheBeadTheBranchCarries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		staleLabels  []string
		branchLabels []string
		wantCommit   bool
	}{
		{"stale assignment is the labeled one", []string{revertwaiver.Label}, nil, false},
		{"branch bead is the labeled one", nil, []string{revertwaiver.Label}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alerts := &checkpointAlertRecorder{}
			bd := newWorkBD(t)
			// The fake lists newest first, so the active-bead fallback picks the
			// stale bead; only the branch pins the bead under test.
			bd.db.Seed(beads.Issue{ID: "gt-branch", Status: "hooked", Assignee: "rig/polecats/polecat",
				Labels: tt.branchLabels, Description: "Delete keep.txt."})
			bd.db.Seed(beads.Issue{ID: "gt-stale", Status: "hooked", Assignee: "rig/polecats/polecat",
				Labels: tt.staleLabels, Description: "Delete keep.txt."})
			d := &Daemon{
				logger:                log.New(io.Discard, "", 0),
				config:                townConfig(t),
				openWorkBeads:         bd.open,
				checkpointRevertAlert: alerts.alert,
			}
			polecat, g := newCheckpointRevertScenarioOn(t, d, "polecat/polecat/gt-branch+mabc123")
			writeWorkFiles(t, polecat, map[string]string{"keep.txt": "keep\n", "wip.txt": "real work in progress\n"})
			before := headOf(t, g)

			created := d.checkpointWorktree(polecat, "rig", "polecat")
			if created != tt.wantCommit {
				t.Fatalf("checkpointWorktree = %v, want %v", created, tt.wantCommit)
			}
			if tt.wantCommit {
				if got := changedSince(t, g, before); !slices.Equal(got, []string{"keep.txt", "wip.txt"}) {
					t.Errorf("checkpoint commit changed %q, want keep.txt and wip.txt", got)
				}
				if len(alerts.calls) != 0 {
					t.Errorf("a directed deletion raised an escalation: %+v", alerts.calls)
				}
				return
			}
			if after := headOf(t, g); after != before {
				t.Errorf("refused checkpoint advanced HEAD to %s, want %s", after, before)
			}
			if len(alerts.calls) != 1 {
				t.Errorf("want exactly one escalation, got %+v", alerts.calls)
			}
		})
	}
}

// TestCheckpointDeletesBySpecUsesTheHookedBeadOnStaleBranchReuse covers a
// redispatched seat hooked to gt-new but checked out on its old gt-old branch.
// gt done submits that tree under the hooked bead, so the dog reads the
// deletes-by-spec waiver from the hooked bead too: a label on only the old
// bead waives nothing, and a label on only the hooked bead waives the deletion
// (gt-aupx1).
func TestCheckpointDeletesBySpecUsesTheHookedBeadOnStaleBranchReuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		oldLabels  []string
		newLabels  []string
		wantCommit bool
	}{
		{"only the old branch bead is labeled", []string{revertwaiver.Label}, nil, false},
		{"only the hooked bead is labeled", nil, []string{revertwaiver.Label}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alerts := &checkpointAlertRecorder{}
			bd := newWorkBD(t)
			bd.db.Seed(beads.Issue{ID: "gt-old", Status: "closed", Assignee: "rig/polecats/polecat",
				Labels: tt.oldLabels, Description: "Delete keep.txt."})
			bd.db.Seed(beads.Issue{ID: "gt-new", Status: "hooked", Assignee: "rig/polecats/polecat",
				Labels: tt.newLabels, Description: "Delete keep.txt."})
			d := &Daemon{
				logger:                log.New(io.Discard, "", 0),
				config:                townConfig(t),
				openWorkBeads:         bd.open,
				checkpointRevertAlert: alerts.alert,
			}
			polecat, g := newCheckpointRevertScenarioOn(t, d, "polecat/polecat/gt-old+mabc123")
			writeWorkFiles(t, polecat, map[string]string{"keep.txt": "keep\n", "wip.txt": "real work in progress\n"})
			before := headOf(t, g)

			created := d.checkpointWorktree(polecat, "rig", "polecat")
			if created != tt.wantCommit {
				t.Fatalf("checkpointWorktree = %v, want %v", created, tt.wantCommit)
			}
			if tt.wantCommit {
				if len(alerts.calls) != 0 {
					t.Errorf("a directed deletion raised an escalation: %+v", alerts.calls)
				}
				return
			}
			if after := headOf(t, g); after != before {
				t.Errorf("refused checkpoint advanced HEAD to %s, want %s", after, before)
			}
			if len(alerts.calls) != 1 {
				t.Errorf("want exactly one escalation, got %+v", alerts.calls)
			}
		})
	}
}

// TestResolveStaleBranchBead pins the dog to gt done's stale-branch guard: the
// branch's issue stands when the seat holds it or its parent, a single other
// assignment replaces it, and an ambiguous or unreadable assignment list names
// no bead.
func TestResolveStaleBranchBead(t *testing.T) {
	t.Parallel()
	seat := "rig/polecats/polecat"
	tests := []struct {
		name       string
		branch     string
		hooked     []string
		progress   []string
		failStatus string
		want       string
		wantErr    bool
	}{
		{"no assignment keeps the branch bead", "gt-old", nil, nil, "", "gt-old", false},
		{"branch bead is the hooked one", "gt-old", []string{"gt-old"}, nil, "", "gt-old", false},
		{"stale branch yields the hooked bead", "gt-old", []string{"gt-new"}, nil, "", "gt-new", false},
		{"stale branch yields the in-progress bead", "gt-old", nil, []string{"gt-new"}, "", "gt-new", false},
		{"subtask branch keeps its own bead", "gt-abc.1", []string{"gt-abc"}, nil, "", "gt-abc.1", false},
		{"branch bead among several assignments", "gt-old", []string{"gt-new", "gt-old"}, nil, "", "gt-old", false},
		{"several assignments, none the branch's", "gt-old", []string{"gt-new", "gt-other"}, nil, "", "", true},
		{"unreadable assignments name no bead", "gt-old", nil, nil, "hooked", "", true},
		{"unreadable hooked list is not padded out by in-progress results", "gt-old", nil, []string{"gt-new"}, "hooked", "", true},
		{"unreadable hooked list is not padded out by the branch's own in-progress bead", "gt-old", nil, []string{"gt-old"}, "hooked", "", true},
		{"unreadable in-progress list after an empty hooked list", "gt-old", nil, nil, "in_progress", "", true},
		{"in-progress list is not read when the hooked list answers", "gt-old", []string{"gt-new"}, nil, "in_progress", "gt-new", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := newWorkBD(t)
			for _, id := range tt.hooked {
				bd.db.Seed(beads.Issue{ID: id, Status: "hooked", Assignee: seat})
			}
			for _, id := range tt.progress {
				bd.db.Seed(beads.Issue{ID: id, Status: "in_progress", Assignee: seat})
			}
			d := &Daemon{logger: log.New(io.Discard, "", 0), config: townConfig(t), openWorkBeads: bd.open}
			if tt.failStatus != "" {
				d.openWorkBeads = func(env []string, timeout time.Duration) workBeadReader {
					return statusFailReader{workBeadReader: bd.open(env, timeout), status: tt.failStatus, err: errors.New("bd down")}
				}
			}

			got, err := d.resolveStaleBranchBead("rig", seat, tt.branch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("bead = %q, want %q", got, tt.want)
			}
		})
	}
}

// statusFailReader fails the list for one status and answers every other read
// through the wrapped reader, so a test can inject the partial answer a real
// bd returns when one query fails.
type statusFailReader struct {
	workBeadReader
	status string
	err    error
}

func (r statusFailReader) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Status == r.status {
		return nil, r.err
	}
	return r.workBeadReader.List(opts)
}
