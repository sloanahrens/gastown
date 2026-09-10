package git

import (
	"os/exec"
	"testing"
)

func TestMergeBase_FindsCommonAncestor(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	cmd := exec.Command("git", "checkout", "-b", "feature", base)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout feature: %v\n%s", err, out)
	}
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	got, err := g.MergeBase(base, head)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if got != base {
		t.Fatalf("MergeBase(base, head) = %q, want %q", got, base)
	}
}
