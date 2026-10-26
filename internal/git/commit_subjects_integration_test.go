//go:build integration

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// CommitSubjects lists the subjects on target..head, newest first, and nothing
// from the target itself (gt-b9j3o.11). It runs real git, so it is an
// integration test.
func TestIntegrationCommitSubjectsOnTargetToHead(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	mainBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if err := g.CreateBranch("feature"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	for _, msg := range []string{"first change", "second change"} {
		if err := os.WriteFile(filepath.Join(dir, msg+".txt"), []byte(msg), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
			t.Fatalf("git add: %v: %s", err, out)
		}
		if out, err := exec.Command("git", "-C", dir, "commit", "-q", "-m", msg).CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v: %s", err, out)
		}
	}

	got, err := g.CommitSubjects(mainBranch, "HEAD")
	if err != nil {
		t.Fatalf("CommitSubjects: %v", err)
	}
	if want := []string{"second change", "first change"}; !slices.Equal(got, want) {
		t.Errorf("CommitSubjects = %q, want %q", got, want)
	}

	none, err := g.CommitSubjects("HEAD", "HEAD")
	if err != nil {
		t.Fatalf("CommitSubjects on an empty range: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("CommitSubjects on an empty range = %q, want none", none)
	}
}
