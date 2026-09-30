package gitfake

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// PathRepo is BranchRepo plus what internal/doctor's checks call: the remote
// list and removal, a rebasing pull, a detached checkout, bare-repository
// and path-state queries, working-tree status and disabling a sparse
// checkout. *git.Git satisfies it, and RunPathContract pins the fake to it.
type PathRepo interface {
	BranchRepo

	Remotes() ([]string, error)
	RemoveRemote(name string) error
	MergeBase(a, b string) (string, error)
	PullRebase() error
	CheckoutDetach(ref string) error
	Status() (*git.GitStatus, error)
	Add(pathspecs ...string) error
	IsBareRepository() (bool, error)
	IsTracked(path string) (bool, error)
	IsIgnored(path string) (bool, error)
	PathChanged(path string) (bool, error)
	UntrackedPaths(pathspec string) ([]string, error)
	DisableSparseCheckout() error
}

var (
	_ PathRepo = (*git.Git)(nil)
	_ PathRepo = (*handle)(nil)
)

func openPathRepo(env BranchEnv, dir string) PathRepo { return env.OpenBranchRepo(dir).(PathRepo) }

// RunPathContract checks the PathRepo behavior every implementation must
// share. A case that fails against *git.Git means the case is wrong; correct
// it to what git does, then make the fake copy it.
func RunPathContract(t *testing.T, newEnv func(t *testing.T) BranchEnv) {
	t.Run("Remotes lists, RemoveRemote drops a remote and its refs", func(t *testing.T) {
		env := newEnv(t)
		fx := newFixture(t, env)
		g := openPathRepo(env, fx.clone)
		up := filepath.Join(fx.root, "up.git")
		env.InitBare(t, up)
		env.Commit(t, up, "main", "up", map[string]string{"u.txt": "u\n"})
		if err := g.AddUpstreamRemote(up); err != nil {
			t.Fatal(err)
		}
		if err := g.FetchBranch("upstream", "main"); err != nil {
			t.Fatal(err)
		}
		if got, err := g.Remotes(); err != nil || !reflect.DeepEqual(got, []string{"origin", "upstream"}) {
			t.Errorf("Remotes = %q, %v", got, err)
		}
		if err := g.RemoveRemote("upstream"); err != nil {
			t.Fatalf("RemoveRemote: %v", err)
		}
		if got, _ := g.Remotes(); !reflect.DeepEqual(got, []string{"origin"}) {
			t.Errorf("Remotes after removal = %q", got)
		}
		if ok, _ := g.RefExists("upstream/main"); ok {
			t.Error("the removed remote's tracking ref survived")
		}
		if err := g.RemoveRemote("upstream"); err == nil {
			t.Error("removing a missing remote succeeded")
		}
	})

	t.Run("MergeBase finds a common ancestor and fails for unrelated history", func(t *testing.T) {
		env := newEnv(t)
		fx := newFixture(t, env)
		g := openPathRepo(env, fx.clone)
		if got, err := g.MergeBase("origin/main", "origin/"+fixtureBranch); err != nil || got != fx.base {
			t.Errorf("MergeBase = %q, %v; want %s", got, err, fx.base)
		}
		other := filepath.Join(fx.root, "other.git")
		env.InitBare(t, other)
		env.Commit(t, other, "main", "unrelated root", map[string]string{"o.txt": "o\n"})
		if err := g.AddUpstreamRemote(other); err != nil {
			t.Fatal(err)
		}
		if err := g.FetchBranch("upstream", "main"); err != nil {
			t.Fatal(err)
		}
		if got, err := g.MergeBase("origin/main", "upstream/main"); err == nil {
			t.Errorf("MergeBase of unrelated histories = %q, want an error", got)
		}
	})

	t.Run("PullRebase fast-forwards to the upstream and needs one", func(t *testing.T) {
		env := newEnv(t)
		fx := newFixture(t, env)
		g := openPathRepo(env, fx.clone)
		later := env.Commit(t, fx.origin, "main", "later", map[string]string{"c.txt": "c\n"})
		if err := g.PullRebase(); err != nil {
			t.Fatalf("PullRebase: %v", err)
		}
		if head, _ := g.Rev("HEAD"); head != later {
			t.Errorf("HEAD after the pull = %s, want %s", head, later)
		}
		if got := readFile(t, filepath.Join(fx.clone, "c.txt")); got != "c\n" {
			t.Errorf("c.txt = %q", got)
		}
		if err := g.PullRebase(); err != nil {
			t.Errorf("PullRebase up to date: %v", err)
		}
		if err := g.CheckoutNewBranch("local", "HEAD"); err != nil {
			t.Fatal(err)
		}
		if err := g.PullRebase(); err == nil {
			t.Error("PullRebase with no upstream succeeded")
		}
	})

	t.Run("CheckoutDetach detaches a checkout and not a bare repository", func(t *testing.T) {
		env := newEnv(t)
		fx := newFixture(t, env)
		g := openPathRepo(env, fx.clone)
		if err := g.CheckoutDetach("origin/" + fixtureBranch); err != nil {
			t.Fatalf("CheckoutDetach: %v", err)
		}
		if b, _ := g.CurrentBranch(); b != "HEAD" {
			t.Errorf("CurrentBranch after detaching = %q, want HEAD", b)
		}
		if head, _ := g.Rev("HEAD"); head != fx.head {
			t.Errorf("HEAD = %s, want %s", head, fx.head)
		}
		bare := env.OpenWithDir(fx.origin, "").(PathRepo)
		if err := bare.CheckoutDetach("main"); err == nil {
			t.Error("detaching a bare repository's HEAD succeeded")
		}
		if isBare, err := bare.IsBareRepository(); err != nil || !isBare {
			t.Errorf("IsBareRepository(bare) = %v, %v", isBare, err)
		}
		if isBare, err := g.IsBareRepository(); err != nil || isBare {
			t.Errorf("IsBareRepository(clone) = %v, %v", isBare, err)
		}
	})

	t.Run("path state: tracked, ignored, changed and untracked", func(t *testing.T) {
		env := newEnv(t)
		fx := newFixture(t, env)
		g := openPathRepo(env, fx.clone)
		writeFile(t, filepath.Join(fx.clone, ".gitignore"), "*.log\n")
		writeFile(t, filepath.Join(fx.clone, "sub", "kept.txt"), "k\n")
		env.CommitWorktree(t, fx.clone, "ignore logs")
		writeFile(t, filepath.Join(fx.clone, "x.log"), "x\n")
		writeFile(t, filepath.Join(fx.clone, "new.txt"), "n\n")
		writeFile(t, filepath.Join(fx.clone, ".beads", "creds"), "secret\n")
		writeFile(t, filepath.Join(fx.clone, ".beads", "db"), "d\n")

		for p, want := range map[string]bool{"a.txt": true, "new.txt": false, "x.log": false, "sub/kept.txt": true} {
			if got, err := g.IsTracked(p); err != nil || got != want {
				t.Errorf("IsTracked(%s) = %v, %v; want %v", p, got, err, want)
			}
		}
		for p, want := range map[string]bool{"x.log": true, "new.txt": false} {
			if got, err := g.IsIgnored(p); err != nil || got != want {
				t.Errorf("IsIgnored(%s) = %v, %v; want %v", p, got, err, want)
			}
		}
		sub := openPathRepo(env, filepath.Join(fx.clone, "sub"))
		if got, err := sub.IsTracked("kept.txt"); err != nil || !got {
			t.Errorf("IsTracked from a subdirectory = %v, %v", got, err)
		}

		if got, err := g.PathChanged("a.txt"); err != nil || got {
			t.Errorf("PathChanged(clean a.txt) = %v, %v", got, err)
		}
		writeFile(t, filepath.Join(fx.clone, "a.txt"), "changed\n")
		if got, err := g.PathChanged("a.txt"); err != nil || !got {
			t.Errorf("PathChanged(modified a.txt) = %v, %v", got, err)
		}
		if err := g.Add("a.txt"); err != nil {
			t.Fatal(err)
		}
		if got, err := g.PathChanged("a.txt"); err != nil || !got {
			t.Errorf("PathChanged(staged a.txt) = %v, %v", got, err)
		}

		if got, err := g.UntrackedPaths(".beads"); err != nil || !reflect.DeepEqual(got, []string{".beads/"}) {
			t.Errorf("UntrackedPaths(.beads) = %q, %v; want the directory", got, err)
		}
		exclude := filepath.Join(fx.clone, ".git", "info", "exclude")
		writeFile(t, exclude, ".beads/\n")
		if got, err := g.UntrackedPaths(".beads"); err != nil || len(got) != 0 {
			t.Errorf("UntrackedPaths(.beads) excluded = %q, %v; want none", got, err)
		}
		if got, err := g.UntrackedPaths("sub"); err != nil || len(got) != 0 {
			t.Errorf("UntrackedPaths(sub) with only tracked files = %q, %v", got, err)
		}
		if _, err := openPathRepo(env, t.TempDir()).IsTracked("a.txt"); err == nil {
			t.Error("IsTracked outside a repository succeeded")
		}
	})

	t.Run("DisableSparseCheckout turns the setting off", func(t *testing.T) {
		env := newEnv(t)
		fx := newFixture(t, env)
		g := openPathRepo(env, fx.clone)
		if err := g.ConfigSet("core.sparseCheckout", "true"); err != nil {
			t.Fatal(err)
		}
		if err := g.DisableSparseCheckout(); err != nil {
			t.Fatalf("DisableSparseCheckout: %v", err)
		}
		if got, _ := g.ConfigGet("core.sparseCheckout"); got == "true" {
			t.Errorf("core.sparseCheckout = %q after disabling", got)
		}
		if _, err := os.Stat(filepath.Join(fx.clone, "a.txt")); err != nil {
			t.Errorf("a.txt gone after disabling: %v", err)
		}
	})
}
