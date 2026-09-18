package polecat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchSurvivingBranches(t *testing.T) {
	tests := []struct {
		name     string
		branches []string
		issue    string
		want     []string
	}{
		{
			name:     "no branches",
			branches: nil,
			issue:    "gt-ibt8",
			want:     nil,
		},
		{
			name:     "empty issue id matches nothing",
			branches: []string{"polecat/flint/gt-ibt8+mabc"},
			issue:    "",
			want:     nil,
		},
		{
			name: "single match",
			branches: []string{
				"polecat/flint/gt-ibt8+mu5wzd6q",
				"polecat/agate/gt-4vbn+mtukyuns",
			},
			issue: "gt-ibt8",
			want:  []string{"polecat/flint/gt-ibt8+mu5wzd6q"},
		},
		{
			name: "newest revision first regardless of polecat name",
			branches: []string{
				// agate's branch is older but sorts first alphabetically
				"polecat/agate/gt-ibt8+mu6jnzt4",
				"polecat/flint/gt-ibt8+mu5wzd6q",
				"polecat/pearl/gt-ibt8+mu72g5cz",
			},
			issue: "gt-ibt8",
			want: []string{
				"polecat/pearl/gt-ibt8+mu72g5cz",
				"polecat/agate/gt-ibt8+mu6jnzt4",
				"polecat/flint/gt-ibt8+mu5wzd6q",
			},
		},
		{
			name: "legacy @ separator is matched",
			branches: []string{
				"polecat/alpha/gt-jns7.1@mk123456",
				"polecat/alpha/gt-other@mk999999",
			},
			issue: "gt-jns7.1",
			want:  []string{"polecat/alpha/gt-jns7.1@mk123456"},
		},
		{
			name: "preserve-then-nuke backup suffix still matches",
			branches: []string{
				"polecat/flint/gt-3s52+mtw13qav",
				"polecat/flint/gt-3s52+backup-8ff13fa",
			},
			issue: "gt-3s52",
			want:  []string{"polecat/flint/gt-3s52+mtw13qav", "polecat/flint/gt-3s52+backup-8ff13fa"},
		},
		{
			name: "branch without issue suffix is ignored",
			branches: []string{
				"polecat/pyrite-mtv8v8s8",
				"polecat/flint/gt-cqy8+mtugedzg",
			},
			issue: "gt-cqy8",
			want:  []string{"polecat/flint/gt-cqy8+mtugedzg"},
		},
		{
			name: "non-polecat branches are ignored",
			branches: []string{
				"main",
				"integration/gt-ibt8",
				"beads-sync",
			},
			issue: "gt-ibt8",
			want:  nil,
		},
		{
			name: "issue prefix must match exactly",
			branches: []string{
				"polecat/flint/gt-ibt8x+mu5wzd6q",
				"polecat/flint/gt-ibt+mu5wzd6q",
			},
			issue: "gt-ibt8",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchSurvivingBranches(tt.branches, tt.issue)
			if len(got) != len(tt.want) {
				t.Fatalf("MatchSurvivingBranches() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("MatchSurvivingBranches()[%d] = %q, want %q (full: %v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestBranchRevision(t *testing.T) {
	tests := []struct {
		branch string
		issue  string
		want   string
	}{
		{"polecat/flint/gt-ibt8+mu5wzd6q", "gt-ibt8", "mu5wzd6q"},
		{"polecat/alpha/gt-jns7.1@mk123456", "gt-jns7.1", "mk123456"},
		{"polecat/flint/gt-3s52+backup-8ff13fa", "gt-3s52", "backup-8ff13fa"},
		{"polecat/pyrite-mtv8v8s8", "gt-ibt8", ""},
		{"polecat/flint/gt-ibt8", "gt-ibt8", ""},
	}
	for _, tt := range tests {
		if got := branchRevision(tt.branch, tt.issue); got != tt.want {
			t.Errorf("branchRevision(%q, %q) = %q, want %q", tt.branch, tt.issue, got, tt.want)
		}
	}
}

// TestFindSurvivingBranchesForIssue_ReadsOrigin is an end-to-end check against
// a real (local, hermetic) git remote: a rig root with the shared bare repo
// layout and an origin holding polecat branches.
func TestFindSurvivingBranchesForIssue_ReadsOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	rigRoot := filepath.Join(tmp, "gastown")

	runGit(t, tmp, "init", "--bare", originDir)
	if err := os.MkdirAll(rigRoot, 0755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}

	// Seed the origin with two branches for the same issue. The refs are
	// written directly so the test needs no commits or worktree.
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "test")
	seedFile(t, filepath.Join(seed, "f.txt"), "hello\n")
	runGit(t, seed, "add", "f.txt")
	runGit(t, seed, "commit", "-m", "seed (gt-ibt8)")
	runGit(t, seed, "branch", "polecat/flint/gt-ibt8+mu5wzd6q")
	runGit(t, seed, "branch", "polecat/pearl/gt-ibt8+mu72g5cz")
	runGit(t, seed, "branch", "polecat/agate/gt-4vbn+mtukyuns")
	runGit(t, seed, "remote", "add", "origin", originDir)
	runGit(t, seed, "push", "origin", "polecat/flint/gt-ibt8+mu5wzd6q", "polecat/pearl/gt-ibt8+mu72g5cz", "polecat/agate/gt-4vbn+mtukyuns")

	// Rig uses the shared bare repo layout (polecat.Manager.repoBase).
	bare := filepath.Join(rigRoot, ".repo.git")
	runGit(t, tmp, "init", "--bare", bare)
	runGit(t, bare, "remote", "add", "origin", originDir)

	got, err := FindSurvivingBranchesForIssue(rigRoot, "gt-ibt8")
	if err != nil {
		t.Fatalf("FindSurvivingBranchesForIssue: %v", err)
	}
	want := []string{"polecat/pearl/gt-ibt8+mu72g5cz", "polecat/flint/gt-ibt8+mu5wzd6q"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("FindSurvivingBranchesForIssue() = %v, want %v", got, want)
	}

	// A bead with no surviving branch reports none, without error.
	none, err := FindSurvivingBranchesForIssue(rigRoot, "gt-nope")
	if err != nil {
		t.Fatalf("FindSurvivingBranchesForIssue(no branch): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected no surviving branches, got %v", none)
	}

	// SurvivingBranchForIssue returns the newest match.
	newest, err := SurvivingBranchForIssue(rigRoot, "gt-ibt8")
	if err != nil {
		t.Fatalf("SurvivingBranchForIssue: %v", err)
	}
	if newest != "polecat/pearl/gt-ibt8+mu72g5cz" {
		t.Errorf("SurvivingBranchForIssue() = %q, want newest branch", newest)
	}

	// No repo at all must error rather than silently report "no branch".
	if _, err := FindSurvivingBranchesForIssue(filepath.Join(tmp, "missing"), "gt-ibt8"); err == nil {
		t.Error("expected error for rig root without a git repo")
	}
}

func seedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}
