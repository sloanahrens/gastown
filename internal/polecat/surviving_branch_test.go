package polecat

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestBranchOnOrigin: the spec dispatcher's resume-branch check reads the
// rig's origin remote, matches the recorded name exactly (a longer branch
// sharing the prefix is not the branch), and calls a rig with no repo an
// error rather than a gone branch (gt-gzhin.3).
func TestBranchOnOrigin(t *testing.T) {
	t.Parallel()
	w := newWorld()
	root := t.TempDir()
	buildCanonicalRigIn(t, w, root)
	mayorRig := filepath.Join(root, "mayor", "rig")
	head := w.rev(t, mayorRig, "main")
	w.SetRef(t, mayorRig, "refs/heads/polecat/agate/gt-a+mu1", head)
	w.SetRef(t, mayorRig, "refs/heads/polecat/agate/gt-a+mu1-suffix", head)

	for _, tc := range []struct {
		branch string
		want   bool
	}{
		{"polecat/agate/gt-a+mu1", true},
		{"polecat/agate/gt-a+mu1-suffix", true},
		{"polecat/agate/gt-a", false},
		{"polecat/agate/gt-a+gone", false},
		{"", false},
	} {
		got, err := branchOnOrigin(w.opener(), root, tc.branch)
		if err != nil {
			t.Fatalf("branchOnOrigin(%q): %v", tc.branch, err)
		}
		if got != tc.want {
			t.Errorf("branchOnOrigin(%q) = %v, want %v", tc.branch, got, tc.want)
		}
	}

	if _, err := branchOnOrigin(w.opener(), t.TempDir(), "polecat/agate/gt-a+mu1"); !errors.Is(err, ErrNoRigRepo) {
		t.Errorf("no-repo error = %v, want ErrNoRigRepo", err)
	}
}

func TestMatchSurvivingBranches(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	rigRoot := filepath.Join(tmp, "gastown")

	// Seed the origin with two branches for the same issue and one for
	// another.
	w := newWorld()
	w.InitBare(t, originDir)
	seed := w.Commit(t, originDir, "main", "seed (gt-ibt8)", map[string]string{"f.txt": "hello\n"})
	for _, b := range []string{"polecat/flint/gt-ibt8+mu5wzd6q", "polecat/pearl/gt-ibt8+mu72g5cz", "polecat/agate/gt-4vbn+mtukyuns"} {
		w.SetRef(t, originDir, "refs/heads/"+b, seed)
	}

	// Rig uses the shared bare repo layout (polecat.Manager.repoBase).
	bare := filepath.Join(rigRoot, ".repo.git")
	w.InitBare(t, bare)
	w.AddRemote(t, bare, "origin", originDir)
	gits := w.opener()

	got, err := findSurvivingBranchesForIssue(gits, rigRoot, "gt-ibt8")
	if err != nil {
		t.Fatalf("FindSurvivingBranchesForIssue: %v", err)
	}
	want := []string{"polecat/pearl/gt-ibt8+mu72g5cz", "polecat/flint/gt-ibt8+mu5wzd6q"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("FindSurvivingBranchesForIssue() = %v, want %v", got, want)
	}

	// A bead with no surviving branch reports none, without error.
	none, err := findSurvivingBranchesForIssue(gits, rigRoot, "gt-nope")
	if err != nil {
		t.Fatalf("FindSurvivingBranchesForIssue(no branch): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected no surviving branches, got %v", none)
	}

	// SurvivingBranchForIssue returns the newest match.
	newest, err := survivingBranchForIssue(gits, rigRoot, "gt-ibt8")
	if err != nil {
		t.Fatalf("SurvivingBranchForIssue: %v", err)
	}
	if newest != "polecat/pearl/gt-ibt8+mu72g5cz" {
		t.Errorf("SurvivingBranchForIssue() = %q, want newest branch", newest)
	}

	// No repo at all must error rather than silently report "no branch".
	if _, err := findSurvivingBranchesForIssue(gits, filepath.Join(tmp, "missing"), "gt-ibt8"); err == nil {
		t.Error("expected error for rig root without a git repo")
	}
}
