//go:build integration

package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the staleness checks against real git repositories and a
// real local remote, pinning the git behaviour the unit tests' fakeGit models:
// fetch refspecs and remote-tracking refs, ancestry, and diff pathspecs.

func realGitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// realGitCommit writes file and commits it, returning the full hash.
func realGitCommit(t *testing.T, dir, file, content string) string {
	t.Helper()
	full := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	realGitRun(t, dir, "add", "-A")
	realGitRun(t, dir, "commit", "-q", "--no-gpg-sign", "-m", "c-"+file)
	return realGitRun(t, dir, "rev-parse", "HEAD")
}

func newRealRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git not available: %v", err)
	}
	dir := t.TempDir()
	realGitRun(t, dir, "init", "-q")
	realGitRun(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

// TestIntegrationCheckStaleBinaryFresh_RefreshesLaggingOriginMain is the
// gt-cq0 regression against real git: the cached origin/main matches the
// binary, the real remote has moved on, and only the live fetch finds out.
func TestIntegrationCheckStaleBinaryFresh_RefreshesLaggingOriginMain(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	realGitRun(t, remoteDir, "init", "-q", "--bare")

	cloneA := newRealRepo(t)
	realGitRun(t, cloneA, "remote", "add", "origin", remoteDir)
	realGitCommit(t, cloneA, "a.go", "1")
	realGitRun(t, cloneA, "branch", "-M", "main")
	realGitRun(t, cloneA, "push", "-q", "origin", "main")

	cloneB := newRealRepo(t)
	realGitRun(t, cloneB, "remote", "add", "origin", remoteDir)
	realGitRun(t, cloneB, "fetch", "-q", "origin", "main")
	realGitRun(t, cloneB, "checkout", "-q", "-b", "main", "origin/main")
	midTip := realGitCommit(t, cloneB, "b.go", "2")
	realGitRun(t, cloneB, "push", "-q", "origin", "main")

	realGitRun(t, cloneA, "fetch", "-q", "origin")
	newTip := realGitCommit(t, cloneB, "c.go", "3")
	realGitRun(t, cloneB, "push", "-q", "origin", "main")

	c := checker{git: realGit, commit: midTip}
	if stale := c.checkStale(cloneA); stale.Error != nil || stale.IsStale {
		t.Fatalf("setup invariant broken: want a (wrong) fresh verdict from the cache, got %+v", stale)
	}

	fresh := c.checkStaleFresh(cloneA)
	if fresh.Error != nil || fresh.Skipped {
		t.Fatalf("Error=%v Skipped=%v (%s), want a verdict", fresh.Error, fresh.Skipped, fresh.SkipReason)
	}
	if !fresh.IsStale || fresh.CompareRef != "origin/main" || fresh.RepoCommit != newTip {
		t.Errorf("got IsStale=%v CompareRef=%q RepoCommit=%s, want stale against origin/main at %s",
			fresh.IsStale, fresh.CompareRef, ShortCommit(fresh.RepoCommit), ShortCommit(newTip))
	}

	// An unreachable origin must fail closed rather than trust the cache.
	unreachable := newRealRepo(t)
	realGitRun(t, unreachable, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist"))
	stalePtr := realGitCommit(t, unreachable, "a.go", "1")
	freshPtr := realGitCommit(t, unreachable, "b.go", "2")
	realGitRun(t, unreachable, "branch", "-M", "main")
	realGitRun(t, unreachable, "update-ref", "refs/heads/main", stalePtr)
	realGitRun(t, unreachable, "update-ref", "refs/remotes/origin/main", freshPtr)
	if info := (checker{git: realGit, commit: freshPtr}).checkStaleFresh(unreachable); !info.Skipped {
		t.Errorf("unreachable origin: got %+v, want fail-closed Skipped", info)
	}
}

// TestIntegrationCheckEmbeddedFormulaDrift_NamesFormulaFiles pins the diff
// pathspecs against real git: only formula source files are named, and a
// .beads/-only advance is not drift.
func TestIntegrationCheckEmbeddedFormulaDrift_NamesFormulaFiles(t *testing.T) {
	t.Parallel()
	dir := newRealRepo(t)
	built := realGitCommit(t, dir, filepath.Join(formulaSourceDir, "mol-polecat-work.formula.toml"), "v1")
	realGitRun(t, dir, "branch", "-M", "main")

	realGitCommit(t, dir, ".beads/issues.jsonl", "backup")
	c := checker{git: realGit, commit: built}
	if info := c.checkStale(dir); info.IsStale {
		t.Errorf("a .beads/-only advance reported stale: %+v", info)
	}

	realGitCommit(t, dir, filepath.Join(formulaSourceDir, "mol-refinery-patrol.formula.toml"), "v2")
	realGitCommit(t, dir, "internal/cmd/formula.go", "// unrelated")

	drift := c.checkFormulaDrift(dir)
	if !drift.Checked {
		t.Fatalf("Checked = false (%s), want a determined result", drift.Reason)
	}
	want := filepath.Join(formulaSourceDir, "mol-refinery-patrol.formula.toml")
	if len(drift.Files) != 1 || drift.Files[0] != want {
		t.Errorf("Files = %v, want [%s]", drift.Files, want)
	}
	if drift.CommitsBehind != 3 {
		t.Errorf("CommitsBehind = %d, want 3", drift.CommitsBehind)
	}
}
