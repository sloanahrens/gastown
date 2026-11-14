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

// TestInitSparseCheckoutRecordsTheSetting drives the fake method rig add calls:
// a repository takes the setting, and a path that is no repository is refused
// (gt-74tzuk).
func TestInitSparseCheckoutRecordsTheSetting(t *testing.T) {
	t.Parallel()
	f := New()
	dir := filepath.Join(t.TempDir(), "clone")
	f.InitRepo(t, dir)
	h := f.Open(dir).(PathRepo)
	if err := h.InitSparseCheckout(dir, []string{"a"}); err != nil {
		t.Fatalf("InitSparseCheckout: %v", err)
	}
	if got, _ := h.ConfigGet("core.sparseCheckout"); got != "true" {
		t.Errorf("core.sparseCheckout = %q after init, want true", got)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if err := h.InitSparseCheckout(missing, nil); err == nil {
		t.Error("InitSparseCheckout outside any repository succeeded")
	}
}

func TestFakeBranchContract(t *testing.T) {
	t.Parallel()
	RunBranchContract(t, func(t *testing.T) BranchEnv { return New() })
}

func TestFakePathContract(t *testing.T) {
	t.Parallel()
	RunPathContract(t, func(t *testing.T) BranchEnv { return New() })
}
