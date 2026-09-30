package gitfake

import (
	"path/filepath"
	"testing"
)

func TestFakeRepoContract(t *testing.T) {
	t.Parallel()
	RunRepoContract(t, func(t *testing.T) Env { return New() })
}

// The inspection helpers a consumer's tests read the world through.
func TestFakeInspection(t *testing.T) {
	t.Parallel()
	f := New()
	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	root := f.Commit(t, origin, "main", "root", map[string]string{"a.txt": "a\n"})
	child := f.Commit(t, origin, "main", "child", map[string]string{"b.txt": "b\n"})
	if got := f.Ref(origin, "refs/heads/main"); got != child {
		t.Errorf("Ref(main) = %s, want %s", got, child)
	}
	if got := f.Parents(child); len(got) != 1 || got[0] != root {
		t.Errorf("Parents(child) = %v, want [%s]", got, root)
	}
	if got := f.Tree(child); got["a.txt"] != "a\n" || got["b.txt"] != "b\n" || len(got) != 2 {
		t.Errorf("Tree(child) = %v", got)
	}
	if f.Parents("nope") != nil || f.Tree("nope") != nil || f.Ref(t.TempDir(), "refs/heads/main") != "" {
		t.Error("unknown ids and repositories answer nil and empty")
	}
}

func TestFakeBranchContract(t *testing.T) {
	t.Parallel()
	RunBranchContract(t, func(t *testing.T) BranchEnv { return New() })
}
