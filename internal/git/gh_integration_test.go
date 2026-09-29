//go:build integration

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationGHSeamRunsGHOnPathInWorkDir is the wiring guard for the gh
// seam: a Git built by NewGit (gh == nil) must run the gh found on PATH, in
// the Git's working directory, with the argv the unit tests assert through
// fakeGH. It covers all three call sites: runGH (LookupPullRequest),
// IsPullRequestApproved and GhPrMergePullRequest.
func TestIntegrationGHSeamRunsGHOnPathInWorkDir(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gh.log")
	script := `#!/bin/sh
printf '%s|%s\n' "$(pwd -P)" "$*" >> "` + logPath + `"
case "$1 $2" in
  "pr view")
    case "$*" in
      *reviewDecision*) printf '%s\n' '{"reviewDecision":"APPROVED"}' ;;
      *) printf '%s\n' '{"number":42,"url":"https://github.com/upstream/repo/pull/42","state":"OPEN","headRefName":"feature","headRefOid":"abc123"}' ;;
    esac
    exit 0 ;;
  "pr merge") exit 0 ;;
esac
printf 'unexpected gh args: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := initTestRepo(t)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGit(dir)
	pr, err := g.LookupPullRequest(PullRequestRef{URL: "https://github.com/upstream/repo/pull/42", TargetRepo: "upstream/repo"})
	if err != nil {
		t.Fatalf("LookupPullRequest: %v", err)
	}
	if pr.Number != 42 || !pr.Open() {
		t.Fatalf("LookupPullRequest = %+v, want open #42", pr)
	}
	approved, err := g.IsPullRequestApproved(pr)
	if err != nil || !approved {
		t.Fatalf("IsPullRequestApproved = %v, %v; want true, nil", approved, err)
	}
	if _, err := g.GhPrMergePullRequest(pr, "squash"); err != nil {
		t.Fatalf("GhPrMergePullRequest: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("gh never ran: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	want := []string{
		realDir + "|pr view https://github.com/upstream/repo/pull/42 --json number,url,state,mergedAt,headRefName,headRefOid,headRepository,headRepositoryOwner",
		realDir + "|pr view https://github.com/upstream/repo/pull/42 --json reviewDecision --repo upstream/repo",
		realDir + "|pr merge https://github.com/upstream/repo/pull/42 --squash --match-head-commit abc123 --repo upstream/repo",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gh invocations:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
