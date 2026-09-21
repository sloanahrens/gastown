package formula

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// conflictCheckSnippet extracts the shell the patrol formula hands the refinery
// for "did the rehearsal merge leave a conflict behind".
func conflictCheckSnippet(t *testing.T) string {
	t.Helper()
	raw, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	content := string(raw)
	const marker = "To detect conflict state after merge fails:"
	i := strings.Index(content, marker)
	if i < 0 {
		t.Fatalf("formula no longer contains %q", marker)
	}
	rest := content[i+len(marker):]
	open := strings.Index(rest, "```bash")
	if open < 0 {
		t.Fatalf("no bash block after %q", marker)
	}
	body := rest[open+len("```bash"):]
	end := strings.Index(body, "```")
	if end < 0 {
		t.Fatalf("unterminated bash block after %q", marker)
	}
	return strings.TrimSpace(body[:end])
}

func requireGitWorktreeSupport(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("snippet is POSIX shell and the premise is a git worktree")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func testGitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	)
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitAllowFail(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func gitAllowFail(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testGitEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// newWorktreeFixture returns a repo and a worktree of branch "other" checked
// out in it. With conflicting=true the branches edit the same file, so merging
// main into the worktree conflicts; otherwise they touch different files.
func newWorktreeFixture(t *testing.T, conflicting bool) (repo, wt string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	wt = filepath.Join(root, "wt")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "f.txt"), "base\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "base")
	gitIn(t, repo, "branch", "other")

	theirs, ours := "g.txt", "h.txt"
	if conflicting {
		theirs, ours = "f.txt", "f.txt"
	}
	gitIn(t, repo, "checkout", "-q", "other")
	writeFile(t, filepath.Join(repo, theirs), "theirs\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "theirs")

	gitIn(t, repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(repo, ours), "ours\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "ours")

	gitIn(t, repo, "worktree", "add", "-q", wt, "other")

	// The whole point rests on this: a worktree's .git is a gitdir pointer
	// file, not a directory. If git ever changes that, reading MERGE_HEAD by
	// path would start working and these tests would pin nothing.
	info, err := os.Lstat(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("lstat worktree .git: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("worktree .git is a directory; the premise of this test changed")
	}
	return repo, wt
}

func runSnippet(t *testing.T, dir, snippet string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", snippet)
	cmd.Dir = dir
	cmd.Env = testGitEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestRefineryConflictDetectorFiresInWorktree runs the formula's own snippet
// against a genuinely conflicted rehearsal inside a worktree. The refinery
// always operates in one, and there .git is a gitdir pointer file rather than a
// directory — a detector reading MERGE_HEAD by path is blind there (gt-cdo3).
func TestRefineryConflictDetectorFiresInWorktree(t *testing.T) {
	requireGitWorktreeSupport(t)
	snippet := conflictCheckSnippet(t)
	_, wt := newWorktreeFixture(t, true)

	mergeOut, err := gitAllowFail(t, wt, "merge", "--no-ff", "--no-edit", "main")
	if err == nil || !strings.Contains(mergeOut, "CONFLICT") {
		t.Fatalf("expected a conflicting rehearsal merge; err=%v out:\n%s", err, mergeOut)
	}

	out, err := runSnippet(t, wt, snippet)
	if err != nil || !strings.Contains(out, "CONFLICT_STATE") {
		t.Fatalf("detector stayed silent on a conflicted merge in a worktree (err=%v, out=%q)\nsnippet:\n%s", err, out, snippet)
	}
}

// TestRefineryConflictDetectorSilentOnCleanMerge is the other half: a detector
// that always fires would route every clean MR into conflict handling.
func TestRefineryConflictDetectorSilentOnCleanMerge(t *testing.T) {
	requireGitWorktreeSupport(t)
	snippet := conflictCheckSnippet(t)
	_, wt := newWorktreeFixture(t, false)

	gitIn(t, wt, "merge", "--no-ff", "--no-edit", "main")

	out, err := runSnippet(t, wt, snippet)
	if err == nil || strings.Contains(out, "CONFLICT_STATE") {
		t.Fatalf("detector reported a conflict after a clean merge (err=%v, out=%q)", err, out)
	}
}
