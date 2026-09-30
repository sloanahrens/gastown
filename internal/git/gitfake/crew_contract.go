package gitfake

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// CrewRepo is what internal/crew needs beyond Repo and WorkTree: cloning on
// the remote's HEAD, remote management, branch creation, the dirty check
// and pull. *git.Git satisfies it, and RunCrewContract pins the fake to it.
type CrewRepo interface {
	Repo
	Clone(url, dest string) error
	CloneWithReference(url, dest, reference string) error
	Remotes() ([]string, error)
	AddRemote(name, url string) (string, error)
	SetRemoteURL(name, url string) (string, error)
	CreateBranch(name string) error
	Checkout(ref string) error
	CurrentBranch() (string, error)
	HasUncommittedChanges() (bool, error)
	Pull(remote, branch string) error
}

var (
	_ CrewRepo = (*git.Git)(nil)
	_ CrewRepo = (*handle)(nil)
)

// RunCrewContract checks the CrewRepo behavior every implementation must
// share, over the same fixtures as RunRepoContract. A case that fails
// against *git.Git means the case is wrong; correct it to what git does, then
// make the fake copy it.
func RunCrewContract(t *testing.T, newEnv func(t *testing.T) Env) {
	t.Run("Clone checks out the remote's HEAD branch with origin set, and clones an empty repository", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		dest := filepath.Join(fx.root, "crew", "dave")
		g := fx.env.Open(fx.root).(CrewRepo)
		if err := g.Clone(fx.origin, dest); err != nil {
			t.Fatalf("Clone: %v", err)
		}
		c := fx.env.Open(dest).(CrewRepo)
		if head, err := c.Rev("HEAD"); err != nil || head != fx.base {
			t.Errorf("clone HEAD = %q, %v; want main %s", head, err, fx.base)
		}
		if branch, err := c.CurrentBranch(); err != nil || branch != "main" {
			t.Errorf("clone branch = %q, %v; want main", branch, err)
		}
		if got := readFile(t, filepath.Join(dest, "a.txt")); got != "one\ntwo\nthree\n" {
			t.Errorf("clone a.txt = %q", got)
		}
		if remotes, err := c.Remotes(); err != nil || !reflect.DeepEqual(remotes, []string{"origin"}) {
			t.Errorf("Remotes = %v, %v; want [origin]", remotes, err)
		}
		if url, err := c.RemoteURL("origin"); err != nil || url != fx.origin {
			t.Errorf("origin = %q, %v; want %s", url, err, fx.origin)
		}
		ref := filepath.Join(fx.root, "crew", "ref")
		if err := g.CloneWithReference(fx.origin, ref, fx.clone); err != nil {
			t.Fatalf("CloneWithReference: %v", err)
		}
		if head, err := fx.env.Open(ref).Rev("HEAD"); err != nil || head != fx.base {
			t.Errorf("reference clone HEAD = %q, %v; want %s", head, err, fx.base)
		}
		empty := filepath.Join(fx.root, "empty.git")
		fx.env.InitBare(t, empty)
		emptyClone := filepath.Join(fx.root, "crew", "empty")
		if err := g.Clone(empty, emptyClone); err != nil {
			t.Fatalf("Clone of an empty repository: %v", err)
		}
		if !fx.env.Open(emptyClone).IsRepo() {
			t.Error("no repository after cloning an empty one")
		}
		if err := g.Clone(filepath.Join(fx.root, "missing.git"), filepath.Join(fx.root, "crew", "x")); err == nil {
			t.Error("Clone of a missing repository succeeded")
		}
	})

	t.Run("AddRemote, SetRemoteURL and Remotes manage remotes; a push URL survives set-url", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		c := fx.env.Open(fx.clone).(CrewRepo)
		up := filepath.Join(fx.root, "upstream.git")
		fork := filepath.Join(fx.root, "fork.git")
		if _, err := c.AddRemote("upstream", up); err != nil {
			t.Fatalf("AddRemote: %v", err)
		}
		if _, err := c.AddRemote("upstream", up); err == nil {
			t.Error("adding an existing remote succeeded")
		}
		if remotes, err := c.Remotes(); err != nil || !reflect.DeepEqual(remotes, []string{"origin", "upstream"}) {
			t.Errorf("Remotes = %v, %v; want [origin upstream]", remotes, err)
		}
		if err := c.ConfigurePushURL("origin", fork); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SetRemoteURL("origin", up); err != nil {
			t.Fatalf("SetRemoteURL: %v", err)
		}
		if url, _ := c.RemoteURL("origin"); url != up {
			t.Errorf("origin after set-url = %q; want %s", url, up)
		}
		if push, _ := c.GetPushURL("origin"); push != fork {
			t.Errorf("origin push URL after set-url = %q; want the kept %s", push, fork)
		}
		if _, err := c.SetRemoteURL("nope", up); err == nil {
			t.Error("SetRemoteURL of a missing remote succeeded")
		}
	})

	t.Run("CreateBranch makes a branch at HEAD; Checkout switches to it", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		c := fx.env.Open(fx.clone).(CrewRepo)
		if err := c.CreateBranch("crew/dave"); err != nil {
			t.Fatalf("CreateBranch: %v", err)
		}
		if err := c.CreateBranch("crew/dave"); err == nil {
			t.Error("creating an existing branch succeeded")
		}
		if branch, _ := c.CurrentBranch(); branch != "main" {
			t.Errorf("CreateBranch switched to %q; it must not check out", branch)
		}
		if err := c.Checkout("crew/dave"); err != nil {
			t.Fatalf("Checkout: %v", err)
		}
		if branch, err := c.CurrentBranch(); err != nil || branch != "crew/dave" {
			t.Errorf("CurrentBranch = %q, %v; want crew/dave", branch, err)
		}
		if head, _ := c.Rev("HEAD"); head != fx.base {
			t.Errorf("new branch HEAD = %s; want %s", head, fx.base)
		}
	})

	t.Run("HasUncommittedChanges sees edits and new files", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		c := fx.env.Open(fx.clone).(CrewRepo)
		if dirty, err := c.HasUncommittedChanges(); err != nil || dirty {
			t.Errorf("fresh clone dirty = %v, %v", dirty, err)
		}
		if err := os.WriteFile(filepath.Join(fx.clone, "new.txt"), []byte("n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if dirty, err := c.HasUncommittedChanges(); err != nil || !dirty {
			t.Errorf("clone with a new file dirty = %v, %v; want true", dirty, err)
		}
	})

	t.Run("Pull fast-forwards to the remote's HEAD branch and refuses a divergence", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		c := fx.env.Open(fx.clone).(CrewRepo)
		next := fx.env.Commit(t, fx.origin, "main", "next", map[string]string{"n.txt": "n\n"})
		if err := c.Pull("origin", ""); err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if head, _ := c.Rev("HEAD"); head != next {
			t.Errorf("HEAD after pull = %s; want %s", head, next)
		}
		if got := readFile(t, filepath.Join(fx.clone, "n.txt")); got != "n\n" {
			t.Errorf("pulled n.txt = %q", got)
		}
		if err := c.Pull("origin", "main"); err != nil {
			t.Errorf("Pull when up to date: %v", err)
		}
		w := fx.env.Open(fx.clone).(WorkTree)
		writeFiles(t, fx.clone, map[string]string{"local.txt": "l\n"})
		if err := w.Add("local.txt"); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit("local"); err != nil {
			t.Fatal(err)
		}
		fx.env.Commit(t, fx.origin, "main", "remote", map[string]string{"r.txt": "r\n"})
		if err := c.Pull("origin", ""); err == nil {
			t.Error("pulling a diverged history succeeded")
		}
	})
}
