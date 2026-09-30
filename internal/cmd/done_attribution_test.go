package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

const claudeTrailer = "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"

func TestStripAttributionLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		message     string
		want        string
		wantRemoved int
	}{
		{
			name:        "co-authored-by trailer",
			message:     "fix: thing (gt-1)\n\nbody line\n\n" + claudeTrailer,
			want:        "fix: thing (gt-1)\n\nbody line",
			wantRemoved: 1,
		},
		{
			name:        "generated-with footer and trailer",
			message:     "fix: thing\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n" + claudeTrailer,
			want:        "fix: thing",
			wantRemoved: 2,
		},
		{
			name:        "trailer case and spacing vary",
			message:     "fix: thing\n\nco-authored-by : Someone <a@b.c>",
			want:        "fix: thing",
			wantRemoved: 1,
		},
		{
			name:        "trailer between other trailers keeps the others",
			message:     "fix: thing\n\nSigned-off-by: A <a@b.c>\n" + claudeTrailer + "\nReviewed-by: B <b@b.c>",
			want:        "fix: thing\n\nSigned-off-by: A <a@b.c>\nReviewed-by: B <b@b.c>",
			wantRemoved: 1,
		},
		{
			name:    "prose that mentions claude is not attribution",
			message: "feat: strip Claude attribution in gt done\n\nClaude Code adds a Co-Authored-By trailer; this removes it.",
		},
		{
			name:    "clean message is returned byte for byte",
			message: "fix: thing\n\n  indented body  \n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, removed := stripAttributionLines(tt.message)
			if len(removed) != tt.wantRemoved {
				t.Errorf("removed %d lines %q, want %d", len(removed), removed, tt.wantRemoved)
			}
			want := tt.want
			if tt.wantRemoved == 0 {
				want = tt.message
			}
			if got != want {
				t.Errorf("cleaned message = %q, want %q", got, want)
			}
		})
	}
}

// newAttributionRepo makes a repo with a base commit on main and a work branch
// checked out on top of it. Commits are added by the caller.
func newAttributionRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	runGitCmd(t, "", "init", "-b", "main", dir)
	gitOut(t, dir, "config", "user.email", "polecat@example.com")
	gitOut(t, dir, "config", "user.name", "Polecat")
	writeTestFile(t, filepath.Join(dir, "README.md"), "# base\n")
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "base")
	gitOut(t, dir, "switch", "-c", "polecat/amber/gt-v4ssj.10")
	return dir
}

func commitFile(t *testing.T, dir, name, message string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, name), name+"\n")
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", message)
}

func TestStripAttributionTrailers_RewritesEveryCommitAndKeepsTheTree(t *testing.T) {
	t.Parallel()
	dir := newAttributionRepo(t)
	commitFile(t, dir, "a.go", "feat: first (gt-1)\n\nwhy\n\n"+claudeTrailer)
	commitFile(t, dir, "b.go", "feat: second (gt-1)\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n"+claudeTrailer)
	commitFile(t, dir, "c.go", "feat: third, already clean (gt-1)")
	g := git.NewGit(dir)
	treeBefore := gitOut(t, dir, "rev-parse", "HEAD^{tree}")
	// Read from the commit itself: the environment's GIT_AUTHOR_* variables
	// override the repo config the fixture sets.
	identityBefore := gitOut(t, dir, "log", "-1", "--format=%an <%ae> %aI %cn <%ce> %cI", "HEAD")

	if err := stripAttributionTrailers(g, "main"); err != nil {
		t.Fatalf("stripAttributionTrailers: %v", err)
	}

	log := gitOut(t, dir, "log", "--format=%B", "main..HEAD")
	for _, banned := range []string{"Co-Authored-By", "anthropic.com", "Generated with"} {
		if strings.Contains(log, banned) {
			t.Errorf("branch messages still contain %q:\n%s", banned, log)
		}
	}
	subjects := gitOut(t, dir, "log", "--reverse", "--format=%s", "main..HEAD")
	if want := "feat: first (gt-1)\nfeat: second (gt-1)\nfeat: third, already clean (gt-1)"; strings.TrimSpace(subjects) != want {
		t.Errorf("subjects = %q, want %q", strings.TrimSpace(subjects), want)
	}
	if body := gitOut(t, dir, "log", "-1", "--format=%b", "HEAD~2"); strings.TrimSpace(body) != "why" {
		t.Errorf("body of the first commit = %q, want the prose kept", body)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD^{tree}"); got != treeBefore {
		t.Errorf("tree changed: %s -> %s", treeBefore, got)
	}
	if status := gitOut(t, dir, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("working tree is dirty after the rewrite:\n%s", status)
	}
	if got := gitOut(t, dir, "log", "-1", "--format=%an <%ae> %aI %cn <%ce> %cI", "HEAD"); got != identityBefore {
		t.Errorf("author/committer not preserved: %q, want %q", got, identityBefore)
	}
	if got := gitOut(t, dir, "rev-list", "--count", "main..HEAD"); strings.TrimSpace(got) != "3" {
		t.Errorf("commit count = %q, want 3", got)
	}
}

func TestStripAttributionTrailers_CleanBranchIsUntouched(t *testing.T) {
	t.Parallel()
	dir := newAttributionRepo(t)
	commitFile(t, dir, "a.go", "feat: first (gt-1)")
	commitFile(t, dir, "b.go", "feat: second (gt-1)\n\nprose about Claude and Co-Authored-By stays")
	g := git.NewGit(dir)
	before := gitOut(t, dir, "rev-parse", "HEAD")

	if err := stripAttributionTrailers(g, "main"); err != nil {
		t.Fatalf("stripAttributionTrailers: %v", err)
	}
	if after := gitOut(t, dir, "rev-parse", "HEAD"); after != before {
		t.Errorf("clean branch was rewritten: %s -> %s", before, after)
	}
}

func TestStripAttributionTrailers_RefusesAttributionSubjectAndLeavesBranchAlone(t *testing.T) {
	t.Parallel()
	dir := newAttributionRepo(t)
	commitFile(t, dir, "a.go", "feat: first (gt-1)\n\n"+claudeTrailer)
	commitFile(t, dir, "b.go", claudeTrailer)
	g := git.NewGit(dir)
	before := gitOut(t, dir, "rev-parse", "HEAD")

	err := stripAttributionTrailers(g, "main")
	if err == nil {
		t.Fatal("stripAttributionTrailers accepted a commit whose subject is the attribution line")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Co-Authored-By") || !strings.Contains(msg, "git commit --amend") {
		t.Errorf("refusal does not show the subject and the fix: %s", msg)
	}
	if after := gitOut(t, dir, "rev-parse", "HEAD"); after != before {
		t.Errorf("refusal moved HEAD: %s -> %s", before, after)
	}
	if got := gitOut(t, dir, "log", "--format=%B", "main..HEAD"); !strings.Contains(got, "Co-Authored-By") {
		t.Errorf("refusal rewrote messages before refusing:\n%s", got)
	}
}

func TestStripAttributionTrailers_RewritesMergeCommit(t *testing.T) {
	t.Parallel()
	dir := newAttributionRepo(t)
	commitFile(t, dir, "a.go", "feat: side (gt-1)\n\n"+claudeTrailer)
	gitOut(t, dir, "switch", "-c", "other", "main")
	commitFile(t, dir, "b.go", "feat: other (gt-1)")
	gitOut(t, dir, "switch", "polecat/amber/gt-v4ssj.10")
	gitOut(t, dir, "merge", "--no-ff", "-m", "merge other (gt-1)\n\n"+claudeTrailer, "other")
	g := git.NewGit(dir)
	treeBefore := gitOut(t, dir, "rev-parse", "HEAD^{tree}")

	if err := stripAttributionTrailers(g, "main"); err != nil {
		t.Fatalf("stripAttributionTrailers: %v", err)
	}
	if got := gitOut(t, dir, "log", "--format=%B", "main..HEAD"); strings.Contains(got, "Co-Authored-By") {
		t.Errorf("trailer survived on the merge branch:\n%s", got)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD^{tree}"); got != treeBefore {
		t.Errorf("tree changed across the merge rewrite")
	}
	if got := gitOut(t, dir, "rev-list", "--parents", "-n", "1", "HEAD"); len(strings.Fields(got)) != 3 {
		t.Errorf("merge commit lost a parent: %q", got)
	}
}
