package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestStalledPolecatCheck_Properties(t *testing.T) {
	check := NewStalledPolecatCheck()

	if check.Name() != "stalled-polecats" {
		t.Errorf("Name() = %q, want %q", check.Name(), "stalled-polecats")
	}

	if check.Description() == "" {
		t.Error("Description() should not be empty")
	}

	if !check.CanFix() {
		t.Error("CanFix() should be true — stalled polecats can have branches pushed")
	}

	if check.Category() != CategoryCleanup {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryCleanup)
	}
}

func TestStalledPolecatCheck_EmptyTownRoot(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir}

	check := NewStalledPolecatCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK for empty town root", result.Status)
	}
}

func TestStalledPolecatCheck_NoPolecats(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}

	check := NewStalledPolecatCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK when no polecats dir exists", result.Status)
	}
}

func TestStalledPolecatCheck_FixNoStalled(t *testing.T) {
	check := NewStalledPolecatCheck()
	// Fix with no stalled polecats should be a no-op
	if err := check.Fix(&CheckContext{TownRoot: t.TempDir()}); err != nil {
		t.Errorf("Fix() with no stalled polecats returned error: %v", err)
	}
}

func TestStalledPolecatCheck_ResolveClonePath_NoDir(t *testing.T) {
	check := NewStalledPolecatCheck()
	path := check.resolveClonePath(t.TempDir(), "testrig", "furiosa")
	if path != "" {
		t.Errorf("resolveClonePath() = %q, want empty for nonexistent", path)
	}
}

// --- gt-4vbn regression fixtures ---
//
// These build a real bare "origin" plus a polecat worktree under the layout
// resolveClonePath expects (townRoot/rigName/polecats/polecatName/rigName),
// so Run/Fix exercise the actual git.BranchTargetStatus content check rather
// than a mock. No tmux session is ever started for the fixture's polecat
// name, so HasSession reports it dead, as gt doctor sees a crashed polecat.

// runGit is defined in branch_check_test.go (same package).

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stalledFixture is a bare origin, a "seed" clone used to play the role of
// other actors (landing content on main, deleting a superseded branch), and
// the polecat's own worktree with one committed-but-unpushed change on a
// properly-named polecat branch.
type stalledFixture struct {
	townRoot    string
	rigName     string
	polecatName string
	issue       string
	branch      string
	bare        string
	seed        string
	clonePath   string
}

func newStalledFixture(t *testing.T, rigName, polecatName, issue string) *stalledFixture {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "origin.git")
	runGit(t, tmp, "init", "--bare", "-q", bare)
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "clone", "-q", bare, seed)
	runGit(t, seed, "checkout", "-q", "-b", "main")
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-q", "-m", "init")
	runGit(t, seed, "push", "-q", "-u", "origin", "main")

	townRoot := filepath.Join(tmp, "town")
	clonePath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		t.Fatalf("mkdir polecat dir: %v", err)
	}
	runGit(t, filepath.Dir(clonePath), "clone", "-q", bare, clonePath)

	branch := polecat.FormatGeneratedBranchName(polecatName, issue, "abc123")
	runGit(t, clonePath, "checkout", "-q", "-b", branch)
	writeFile(t, filepath.Join(clonePath, "work.txt"), "polecat work\n")
	runGit(t, clonePath, "add", "work.txt")
	runGit(t, clonePath, "commit", "-q", "-m", "do the work ("+issue+")")

	return &stalledFixture{
		townRoot: townRoot, rigName: rigName, polecatName: polecatName,
		issue: issue, branch: branch, bare: bare, seed: seed, clonePath: clonePath,
	}
}

// land pushes the polecat's branch, merges it into main from a different
// clone (the "seed"), and then deletes the branch on origin from that same
// seed clone — mirroring the incident in gt-4vbn: the deacon patrol deleted
// agate's branch on origin from its own clone, leaving agate's worktree with
// a local tracking ref for a branch that no longer exists remotely, while the
// content is safely on main under a different commit.
func (f *stalledFixture) land(t *testing.T) {
	t.Helper()
	runGit(t, f.clonePath, "push", "-q", "-u", "origin", f.branch)
	runGit(t, f.seed, "fetch", "-q", "origin", f.branch)
	runGit(t, f.seed, "merge", "-q", "--no-ff", "origin/"+f.branch, "-m", "merge "+f.branch)
	runGit(t, f.seed, "push", "-q", "origin", "main")
	runGit(t, f.seed, "push", "-q", "origin", "--delete", f.branch)
}

func TestStalledPolecatCheck_FlagsGenuinelyUnpushedWork(t *testing.T) {
	f := newStalledFixture(t, "testrig", "test-furiosa", "gt-open1")
	ctx := &CheckContext{TownRoot: f.townRoot, RigName: f.rigName}

	check := NewStalledPolecatCheck()
	check.beadStatus = func(string, string) (string, bool) { return "open", true }
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning for genuinely unpushed work", result.Status)
	}
	if len(check.stalledPolecats) != 1 || check.stalledPolecats[0].branch != f.branch {
		t.Fatalf("stalledPolecats = %+v, want one entry for branch %q", check.stalledPolecats, f.branch)
	}
}

// TestStalledPolecatCheck_SkipsContentAlreadyOnDefaultBranch is the direct
// gt-4vbn regression: a branch deleted on origin by another actor, whose
// commit landed on main under a different SHA, must not be flagged — even
// though the exact branch is gone from origin and the polecat clone's own
// (now dangling) tracking ref for it still resolves locally.
func TestStalledPolecatCheck_SkipsContentAlreadyOnDefaultBranch(t *testing.T) {
	f := newStalledFixture(t, "testrig", "test-agate", "gt-landed1")
	f.land(t)
	ctx := &CheckContext{TownRoot: f.townRoot, RigName: f.rigName}

	check := NewStalledPolecatCheck()
	check.beadStatus = func(string, string) (string, bool) { return "open", true }
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK — branch content is already on main (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 0 {
		t.Fatalf("stalledPolecats = %+v, want none — content already landed", check.stalledPolecats)
	}
}

func TestStalledPolecatCheck_SkipsWhenBeadClosed(t *testing.T) {
	f := newStalledFixture(t, "testrig", "furiosa", "gt-closed1")
	ctx := &CheckContext{TownRoot: f.townRoot, RigName: f.rigName}

	check := NewStalledPolecatCheck()
	check.beadStatus = func(_ string, beadID string) (string, bool) {
		if beadID == f.issue {
			return "closed", true
		}
		return "", false
	}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK — bead behind the branch is closed (details: %v)", result.Status, result.Details)
	}
	if len(check.stalledPolecats) != 0 {
		t.Fatalf("stalledPolecats = %+v, want none — bead closed", check.stalledPolecats)
	}
}

// TestStalledPolecatCheck_Fix_RefusesToResurrectLandedContent guards the
// second half of gt-4vbn: "the suggested fix is actively harmful" — Fix must
// re-verify at push time, not just trust Run's snapshot. It flags the branch
// while genuinely unpreserved, lands the content on main afterward (as if
// another polecat merged it during the gap between Run and Fix), and asserts
// Fix does not push the now-superseded branch back to origin.
func TestStalledPolecatCheck_Fix_RefusesToResurrectLandedContent(t *testing.T) {
	f := newStalledFixture(t, "testrig", "test-agate2", "gt-race1")
	ctx := &CheckContext{TownRoot: f.townRoot, RigName: f.rigName}

	check := NewStalledPolecatCheck()
	check.beadStatus = func(string, string) (string, bool) { return "open", true }
	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("Run() Status = %v, want Warning before the race", result.Status)
	}
	if len(check.stalledPolecats) != 1 {
		t.Fatalf("expected exactly one stalled polecat before the race, got %+v", check.stalledPolecats)
	}

	// Simulate the race: content lands on main, and the branch is deleted on
	// origin, between Run and Fix — without going through check.Run again.
	f.land(t)

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error = %v, want nil (superseded branch, nothing to push)", err)
	}

	out := runGit(t, f.seed, "ls-remote", "--heads", "origin", f.branch)
	if out != "" {
		t.Fatalf("Fix() pushed a superseded branch back to origin: %q", out)
	}
}

func TestStalledPolecatCheck_BranchSupersededByClosedBead(t *testing.T) {
	check := NewStalledPolecatCheck()

	branch := polecat.FormatGeneratedBranchName("furiosa", "gt-999", "abc123")

	tests := []struct {
		name   string
		status func(string, string) (string, bool)
		branch string
		want   bool
	}{
		{"closed bead", func(string, string) (string, bool) { return "closed", true }, branch, true},
		{"tombstoned bead", func(string, string) (string, bool) { return "tombstone", true }, branch, true},
		{"open bead", func(string, string) (string, bool) { return "open", true }, branch, false},
		{"lookup failed", func(string, string) (string, bool) { return "", false }, branch, false},
		{"branch has no encoded issue", func(string, string) (string, bool) { return "closed", true }, "polecat/furiosa-abc123", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check.beadStatus = tt.status
			if got := check.branchSupersededByClosedBead("/irrelevant", tt.branch); got != tt.want {
				t.Errorf("branchSupersededByClosedBead() = %v, want %v", got, tt.want)
			}
		})
	}
}
