package gitfake

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// BranchRepo is Repo plus what a consumer that manages worktrees and
// branches needs (internal/polecat): worktree and branch lifecycle, remote
// listings, config, and the uncommitted-work and preservation verdicts.
// *git.Git satisfies it, and RunBranchContract pins the fake to it.
type BranchRepo interface {
	Repo
	git.IndexSkewClassifier

	TopLevel() (string, error)
	GitDir() (string, error)
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error

	RefExists(ref string) (bool, error)
	FirstParentContains(commit, descendant string) (bool, error)
	Cherry(upstream, head string) (string, error)
	CountCommitsBehind(ref string) (int, error)
	CurrentBranch() (string, error)
	ListBranches(pattern string) ([]string, error)
	DeleteBranch(name string, force bool) error
	RemoteDefaultBranch() string

	Fetch(remote string) error
	FetchBranch(remote, branch string) error
	FetchPrune(remote string) error
	Push(remote, branch string, force bool) error
	DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error
	GC() error
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	ListRemoteRefsWithHashesTimeout(remote, prefix string, timeout time.Duration) ([]git.RemoteRef, error)

	Checkout(ref string) error
	CheckoutNewBranch(branch, startPoint string) error
	CheckoutResetBranch(branch, startPoint string) error
	CheckoutDetachForce(ref string) error
	ResetHard(ref string) error
	CleanForce() error

	WorktreeAddFromRef(path, branch, startPoint string) error
	WorktreeAddExistingForce(path, branch string) error
	WorktreeMove(oldPath, newPath string) error
	WorktreeList() ([]git.Worktree, error)

	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
	CheckUncommittedWorkLocal() (*git.UncommittedWorkStatus, error)
	BranchPushedToRemote(localBranch, remote string) (bool, int, error)
	BranchPreservationStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error)
	BranchTargetStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error)
	RefPreservedByRef(head, ref string) (git.BranchPreservationStatus, error)
}

var (
	_ BranchRepo = (*git.Git)(nil)
	_ BranchRepo = (*handle)(nil)
)

// BranchEnv is Env plus the fixtures RunBranchContract needs: commits
// made from a checkout's files, stashes, and opening a bare repository by
// its git directory.
type BranchEnv interface {
	Env
	// CommitWorktree commits the checkout at dir as it is on disk onto its
	// HEAD and returns the commit's id.
	CommitWorktree(t testing.TB, dir, message string) string
	// Stash stashes the checkout at dir's tracked changes with message.
	Stash(t testing.TB, dir, message string)
	// OpenBranchRepo opens the repository or worktree at dir.
	OpenBranchRepo(dir string) BranchRepo
}

var _ BranchEnv = (*Fake)(nil)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wtFixture is fixture plus the clone opened with the wider surface.
func newWtFixture(t *testing.T, env BranchEnv) (*fixture, BranchRepo) {
	t.Helper()
	fx := newFixture(t, env)
	return fx, env.OpenBranchRepo(fx.clone)
}

// RunBranchContract checks the BranchRepo behavior every implementation
// must share. A case that fails against *git.Git means the case is wrong;
// correct it to what git does, then make the fake copy it.
func RunBranchContract(t *testing.T, newEnv func(t *testing.T) BranchEnv) {
	t.Run("TopLevel and GitDir for a clone, a subdirectory and a linked worktree", func(t *testing.T) {
		t.Parallel()
		env := newEnv(t)
		fx, g := newWtFixture(t, env)
		top, err := g.TopLevel()
		if err != nil || !samePath(top, fx.clone) {
			t.Errorf("TopLevel = %q, %v; want %s", top, err, fx.clone)
		}
		sub := filepath.Join(fx.clone, "sub", "deeper")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if top, err := env.OpenBranchRepo(sub).TopLevel(); err != nil || !samePath(top, fx.clone) {
			t.Errorf("TopLevel from a subdirectory = %q, %v; want the enclosing %s", top, err, fx.clone)
		}
		if gd, err := g.GitDir(); err != nil || !samePath(gd, filepath.Join(fx.clone, ".git")) {
			t.Errorf("GitDir = %q, %v", gd, err)
		}
		wt := filepath.Join(fx.root, "linked")
		if err := g.WorktreeAddFromRef(wt, "linked-branch", "origin/main"); err != nil {
			t.Fatal(err)
		}
		lg := env.OpenBranchRepo(wt)
		if top, err := lg.TopLevel(); err != nil || !samePath(top, wt) {
			t.Errorf("linked TopLevel = %q, %v", top, err)
		}
		if gd, err := lg.GitDir(); err != nil || samePath(gd, filepath.Join(fx.clone, ".git")) {
			t.Errorf("linked GitDir = %q, %v; want its own directory", gd, err)
		} else if _, err := os.Stat(gd); err != nil {
			t.Errorf("linked GitDir %s does not exist: %v", gd, err)
		}
		if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
			t.Errorf("linked worktree has no .git: %v", err)
		}
		if _, err := env.OpenWithDir(fx.origin, "").(BranchRepo).TopLevel(); err == nil {
			t.Error("TopLevel of a bare repository succeeded")
		}
		if _, err := env.OpenBranchRepo(t.TempDir()).TopLevel(); err == nil {
			t.Error("TopLevel outside a repository succeeded")
		}
	})

	t.Run("config set and get", func(t *testing.T) {
		t.Parallel()
		_, g := newWtFixture(t, newEnv(t))
		if v, err := g.ConfigGet("beads.role"); err != nil || v != "" {
			t.Errorf("unset key = %q, %v", v, err)
		}
		if err := g.ConfigSet("beads.role", "contributor"); err != nil {
			t.Fatal(err)
		}
		if v, _ := g.ConfigGet("beads.role"); v != "contributor" {
			t.Errorf("ConfigGet after set = %q", v)
		}
	})

	t.Run("refs, branches and the default branch", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		for ref, want := range map[string]bool{
			"refs/remotes/origin/main": true, "origin/" + fixtureBranch: true, "main": true,
			"refs/heads/nope": false, "origin/nope": false,
		} {
			if got, err := g.RefExists(ref); err != nil || got != want {
				t.Errorf("RefExists(%s) = %v, %v; want %v", ref, got, err, want)
			}
		}
		if b, err := g.CurrentBranch(); err != nil || b != "main" {
			t.Errorf("CurrentBranch = %q, %v", b, err)
		}
		if d := g.RemoteDefaultBranch(); d != "main" {
			t.Errorf("RemoteDefaultBranch = %q", d)
		}
		for _, b := range []string{"polecat/a", "polecat/b", "other"} {
			if err := g.CheckoutNewBranch(b, "main"); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := g.ListBranches("polecat/*"); err != nil || !reflect.DeepEqual(got, []string{"polecat/a", "polecat/b"}) {
			t.Errorf("ListBranches(polecat/*) = %v, %v", got, err)
		}
		if err := g.CheckoutNewBranch("other", "main"); err == nil {
			t.Error("CheckoutNewBranch over an existing branch succeeded")
		}
		if err := g.DeleteBranch("other", false); err == nil {
			t.Error("deleting the checked-out branch succeeded")
		}
		if err := g.Checkout("main"); err != nil {
			t.Fatal(err)
		}
		if err := g.DeleteBranch("other", false); err != nil {
			t.Errorf("deleting a merged branch: %v", err)
		}
		if err := g.CheckoutNewBranch("ahead", "main"); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(fx.clone, "ahead.txt"), "ahead\n")
		fx.env.(BranchEnv).CommitWorktree(t, fx.clone, "ahead")
		if err := g.Checkout("main"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(fx.clone, "ahead.txt")); !os.IsNotExist(err) {
			t.Errorf("checking out main left ahead.txt: %v", err)
		}
		if err := g.DeleteBranch("ahead", false); err == nil {
			t.Error("deleting an unmerged branch without force succeeded")
		}
		if err := g.DeleteBranch("ahead", true); err != nil {
			t.Errorf("force-deleting an unmerged branch: %v", err)
		}
		if err := g.DeleteBranch("gone", true); err == nil {
			t.Error("deleting a missing branch succeeded")
		}
	})

	t.Run("fetch, push and remote listings", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		env := fx.env.(BranchEnv)
		later := env.Commit(t, fx.origin, "polecat/x", "x", map[string]string{"x.txt": "x\n"})
		if err := g.FetchBranch("origin", "polecat/x"); err != nil {
			t.Fatal(err)
		}
		if id, _ := g.Rev("origin/polecat/x"); id != later {
			t.Errorf("after FetchBranch origin/polecat/x = %s, want %s", id, later)
		}
		if err := g.FetchBranch("origin", "gone"); err == nil {
			t.Error("FetchBranch of a missing branch succeeded")
		}
		moved := env.Commit(t, fx.origin, "main", "main moves", map[string]string{"m.txt": "m\n"})
		if err := g.Fetch("origin"); err != nil {
			t.Fatal(err)
		}
		if id, _ := g.Rev("origin/main"); id != moved {
			t.Errorf("after Fetch origin/main = %s, want %s", id, moved)
		}
		refs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/polecat/")
		if err != nil || len(refs) != 1 || refs[0].Name != "refs/heads/polecat/x" || refs[0].Hash != later {
			t.Errorf("ListRemoteRefsWithHashes = %+v, %v", refs, err)
		}
		if err := g.CheckoutNewBranch("polecat/y", "origin/main"); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(fx.clone, "y.txt"), "y\n")
		y := env.CommitWorktree(t, fx.clone, "y")
		if err := g.Push("origin", "polecat/y", false); err != nil {
			t.Fatalf("Push: %v", err)
		}
		if tip, _ := g.PushRemoteBranchTip("origin", "polecat/y"); tip != y {
			t.Errorf("remote polecat/y = %s, want %s", tip, y)
		}
		if id, _ := g.Rev("origin/polecat/y"); id != y {
			t.Errorf("push did not update origin/polecat/y: %s", id)
		}
		if err := g.CheckoutResetBranch("polecat/y", "origin/main"); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(fx.clone, "z.txt"), "z\n")
		env.CommitWorktree(t, fx.clone, "z")
		if err := g.Push("origin", "polecat/y", false); err == nil {
			t.Error("a non-fast-forward push without force succeeded")
		}
		if err := g.Push("origin", "polecat/y", true); err != nil {
			t.Errorf("forced push: %v", err)
		}
	})

	t.Run("lease-guarded remote delete, fetch --prune and gc", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		if _, err := g.Rev("origin/" + fixtureBranch); err != nil {
			t.Fatalf("clone has no origin/%s: %v", fixtureBranch, err)
		}
		if err := g.DeleteRemoteBranchIfAt("origin", fixtureBranch, fx.base); err == nil {
			t.Error("delete with a stale lease succeeded")
		}
		if err := g.DeleteRemoteBranchIfAt("origin", fixtureBranch, fx.head); err != nil {
			t.Fatalf("delete at the remote's tip: %v", err)
		}
		if tip, _ := g.PushRemoteBranchTip("origin", fixtureBranch); tip != "" {
			t.Errorf("remote %s still at %s after delete", fixtureBranch, tip)
		}
		if ok, _ := g.RefExists("refs/remotes/origin/" + fixtureBranch); ok {
			t.Error("delete left the remote-tracking ref")
		}

		env := fx.env.(BranchEnv)
		gone := env.Commit(t, fx.origin, "gone", "gone", map[string]string{"g.txt": "g\n"})
		if err := g.Fetch("origin"); err != nil {
			t.Fatal(err)
		}
		if id, _ := g.Rev("origin/gone"); id != gone {
			t.Fatalf("origin/gone = %s, want %s", id, gone)
		}
		other := filepath.Join(fx.root, "other")
		env.Clone(t, fx.origin, other)
		if err := env.OpenBranchRepo(other).DeleteRemoteBranchIfAt("origin", "gone", gone); err != nil {
			t.Fatal(err)
		}
		if err := g.FetchPrune("origin"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := g.RefExists("refs/remotes/origin/gone"); ok {
			t.Error("FetchPrune kept a ref the remote no longer has")
		}
		if ok, _ := g.RefExists("refs/remotes/origin/main"); !ok {
			t.Error("FetchPrune dropped origin/main")
		}
		if err := g.GC(); err != nil {
			t.Errorf("GC: %v", err)
		}
		if err := env.OpenBranchRepo(fx.root).GC(); err == nil {
			t.Error("GC outside a repository succeeded")
		}
	})

	t.Run("worktrees: add from a ref, add existing, list, move, remove", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		a := filepath.Join(fx.root, "wts", "a")
		if err := g.WorktreeAddFromRef(a, "polecat/a", "origin/main"); err != nil {
			t.Fatal(err)
		}
		if err := g.WorktreeAddFromRef(filepath.Join(fx.root, "wts", "a2"), "polecat/a", "origin/main"); err == nil {
			t.Error("adding a worktree on an existing branch name with -b succeeded")
		}
		if got := readFile(t, filepath.Join(a, "a.txt")); got != "one\ntwo\nthree\n" {
			t.Errorf("a.txt = %q", got)
		}
		b := filepath.Join(fx.root, "wts", "b")
		if err := g.WorktreeAddExistingForce(b, "polecat/a"); err != nil {
			t.Fatalf("WorktreeAddExistingForce on a held branch: %v", err)
		}
		list, err := g.WorktreeList()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, w := range list {
			got[resolved(w.Path)] = w.Branch
		}
		want := map[string]string{resolved(fx.clone): "main", resolved(a): "polecat/a", resolved(b): "polecat/a"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("WorktreeList = %v, want %v", got, want)
		}
		c := filepath.Join(fx.root, "wts", "c")
		if err := g.WorktreeMove(b, c); err != nil {
			t.Fatalf("WorktreeMove: %v", err)
		}
		if b, err := fx.env.(BranchEnv).OpenBranchRepo(c).CurrentBranch(); err != nil || b != "polecat/a" {
			t.Errorf("moved worktree branch = %q, %v", b, err)
		}
		if err := g.WorktreeRemove(c, true); err != nil {
			t.Fatal(err)
		}
		list, _ = g.WorktreeList()
		if len(list) != 2 {
			t.Errorf("after remove WorktreeList = %+v", list)
		}
		bare := fx.env.OpenWithDir(fx.origin, "").(BranchRepo)
		d := filepath.Join(fx.root, "wts", "d")
		if err := bare.WorktreeAddFromRef(d, "polecat/d", "main"); err != nil {
			t.Fatalf("worktree from a bare repository: %v", err)
		}
		if b, _ := fx.env.(BranchEnv).OpenBranchRepo(d).CurrentBranch(); b != "polecat/d" {
			t.Errorf("bare repo's worktree branch = %q", b)
		}
	})

	t.Run("checkout, reset and clean keep untracked files as git does", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		writeFile(t, filepath.Join(fx.clone, ".gitignore"), "ignored.txt\n")
		fx.env.(BranchEnv).CommitWorktree(t, fx.clone, "ignore")
		// info/exclude in the common git dir ignores like .gitignore does.
		writeFile(t, filepath.Join(fx.clone, ".git", "info", "exclude"), ".runtime/\n")
		writeFile(t, filepath.Join(fx.clone, ".runtime", "state"), "x\n")
		writeFile(t, filepath.Join(fx.clone, "a.txt"), "edited\n")
		writeFile(t, filepath.Join(fx.clone, "untracked.txt"), "u\n")
		writeFile(t, filepath.Join(fx.clone, "newdir", "n.txt"), "n\n")
		writeFile(t, filepath.Join(fx.clone, "ignored.txt"), "i\n")
		st, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatal(err)
		}
		if !st.HasUncommittedChanges || !reflect.DeepEqual(st.ModifiedFiles, []string{"a.txt"}) || !reflect.DeepEqual(sorted(st.UntrackedFiles), []string{"newdir/n.txt", "untracked.txt"}) {
			t.Errorf("status: changes=%v modified=%q untracked=%q", st.HasUncommittedChanges, st.ModifiedFiles, st.UntrackedFiles)
		}
		if err := g.ResetHard("HEAD"); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, filepath.Join(fx.clone, "a.txt")); got != "one\ntwo\nthree\n" {
			t.Errorf("a.txt after reset = %q", got)
		}
		if _, err := os.Stat(filepath.Join(fx.clone, "untracked.txt")); err != nil {
			t.Errorf("reset --hard removed an untracked file: %v", err)
		}
		if err := g.CleanForce(); err != nil {
			t.Fatal(err)
		}
		for p, want := range map[string]bool{"untracked.txt": false, "newdir": false, "ignored.txt": true} {
			_, err := os.Stat(filepath.Join(fx.clone, p))
			if (err == nil) != want {
				t.Errorf("after clean, %s exists = %v, want %v", p, err == nil, want)
			}
		}
		if st, _ := g.CheckUncommittedWork(); st.HasUncommittedChanges {
			t.Errorf("status after reset and clean = %+v", st)
		}
		// A switch that would overwrite a local edit is refused, and leaves
		// the checkout where it was.
		writeFile(t, filepath.Join(fx.clone, "b.txt"), "local edit\n")
		if err := g.CheckoutNewBranch("from-head", fx.head); err == nil {
			t.Error("checkout over an untracked file the target tracks succeeded")
		}
		if b, _ := g.CurrentBranch(); b != "main" {
			t.Errorf("refused checkout moved HEAD to %q", b)
		}
		if err := os.Remove(filepath.Join(fx.clone, "b.txt")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(fx.clone, "a.txt"), "dirty\n")
		env := fx.env.(BranchEnv)
		env.Commit(t, fx.origin, "main", "change a", map[string]string{"a.txt": "changed upstream\n"})
		if err := g.Fetch("origin"); err != nil {
			t.Fatal(err)
		}
		if err := g.CheckoutNewBranch("upstream-a", "origin/main"); err == nil {
			t.Error("checkout over a locally edited file the switch changes succeeded")
		}
		if err := g.CheckoutNewBranch("keeps-edit", "HEAD"); err != nil {
			t.Errorf("a switch that leaves the edited file alone was refused: %v", err)
		}
		if got := readFile(t, filepath.Join(fx.clone, "a.txt")); got != "dirty\n" {
			t.Errorf("the local edit did not survive the switch: %q", got)
		}
		if err := g.CheckoutDetachForce(fx.head); err != nil {
			t.Fatal(err)
		}
		if b, _ := g.CurrentBranch(); b != "HEAD" {
			t.Errorf("CurrentBranch detached = %q", b)
		}
		if got := readFile(t, filepath.Join(fx.clone, "b.txt")); got != "work\n" {
			t.Errorf("b.txt at the detached head = %q", got)
		}
	})

	t.Run("stashes count on their own branch", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		env := fx.env.(BranchEnv)
		writeFile(t, filepath.Join(fx.clone, "a.txt"), "stash me\n")
		env.Stash(t, fx.clone, "on main")
		if got := readFile(t, filepath.Join(fx.clone, "a.txt")); got != "one\ntwo\nthree\n" {
			t.Errorf("a.txt after stash = %q", got)
		}
		if st, _ := g.CheckUncommittedWork(); st.StashCount != 1 || st.HasUncommittedChanges {
			t.Errorf("status on main = %+v", st)
		}
		if err := g.CheckoutNewBranch("elsewhere", "main"); err != nil {
			t.Fatal(err)
		}
		if st, _ := g.CheckUncommittedWork(); st.StashCount != 0 {
			t.Errorf("another branch counts main's stash: %+v", st)
		}
	})

	t.Run("history queries: Cherry, FirstParentContains, CountCommitsBehind", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		env := fx.env.(BranchEnv)
		replay := env.Commit(t, fx.origin, "main", "replay b", map[string]string{"b.txt": "work\n"})
		extra := env.Commit(t, fx.origin, fixtureBranch, "extra", map[string]string{"e.txt": "e\n"})
		if err := g.Fetch("origin"); err != nil {
			t.Fatal(err)
		}
		out, err := g.Cherry("origin/main", "origin/"+fixtureBranch)
		if err != nil || out != "- "+fx.head+"\n+ "+extra {
			t.Errorf("Cherry = %q, %v", out, err)
		}
		if ok, err := g.FirstParentContains(fx.base, replay); err != nil || !ok {
			t.Errorf("FirstParentContains(base, main) = %v, %v", ok, err)
		}
		if ok, _ := g.FirstParentContains(fx.head, replay); ok {
			t.Error("the branch head is on main's first-parent line")
		}
		if n, err := g.CountCommitsBehind("origin/main"); err != nil || n != 1 {
			t.Errorf("CountCommitsBehind = %d, %v; want 1", n, err)
		}
	})

	t.Run("preservation verdicts", func(t *testing.T) {
		t.Parallel()
		fx, g := newWtFixture(t, newEnv(t))
		env := fx.env.(BranchEnv)
		wtPath := filepath.Join(fx.root, "pc")
		if err := g.WorktreeAddFromRef(wtPath, "polecat/p", "origin/main"); err != nil {
			t.Fatal(err)
		}
		pc := env.OpenBranchRepo(wtPath)
		writeFile(t, filepath.Join(wtPath, "p.txt"), "p\n")
		env.CommitWorktree(t, wtPath, "p work")

		st, err := pc.CheckUncommittedWork()
		if err != nil || st.UnpushedCommits != 1 || st.HasUncommittedChanges {
			t.Errorf("unpushed work: %+v, %v", st, err)
		}
		if ok, n, err := pc.BranchPushedToRemote("polecat/p", "origin"); err != nil || ok || n != 1 {
			t.Errorf("BranchPushedToRemote before push = %v %d %v", ok, n, err)
		}
		if err := pc.Push("origin", "polecat/p", false); err != nil {
			t.Fatal(err)
		}
		if ok, _, err := pc.BranchPushedToRemote("polecat/p", "origin"); err != nil || !ok {
			t.Errorf("BranchPushedToRemote after push = %v %v", ok, err)
		}
		if st, _ := pc.CheckUncommittedWork(); st.UnpushedCommits != 0 {
			t.Errorf("pushed work still unpushed: %+v", st)
		}
		// Target status ignores the pushed branch: the work is not on main.
		ts, err := pc.BranchTargetStatus("polecat/p", "origin", []string{"main"})
		if err != nil || ts.Preserved || ts.UnpreservedPatchCount != 1 {
			t.Errorf("BranchTargetStatus against main = %+v, %v", ts, err)
		}
		// Squash-landing the change on main preserves it by patch.
		env.Commit(t, fx.origin, "main", "squash p", map[string]string{"p.txt": "p\n"})
		if err := pc.Fetch("origin"); err != nil {
			t.Fatal(err)
		}
		ts, err = pc.BranchTargetStatus("polecat/p", "origin", []string{"main"})
		if err != nil || !ts.Preserved {
			t.Errorf("BranchTargetStatus after the squash = %+v, %v", ts, err)
		}
		rp, err := pc.RefPreservedByRef("HEAD", "origin/main")
		if err != nil || !rp.Preserved {
			t.Errorf("RefPreservedByRef = %+v, %v", rp, err)
		}
		if rp, _ := pc.RefPreservedByRef("origin/"+fixtureBranch, "origin/main"); rp.Preserved {
			t.Errorf("the unlanded branch reads preserved: %+v", rp)
		}
		// A detached HEAD at a pushed branch tip is in custody.
		if err := pc.CheckoutDetachForce("origin/polecat/p"); err != nil {
			t.Fatal(err)
		}
		bp, err := pc.BranchPreservationStatus("", "origin", nil)
		if err != nil || !bp.Preserved {
			t.Errorf("detached at a pushed tip = %+v, %v", bp, err)
		}
		if ls, err := pc.CheckUncommittedWorkLocal(); err != nil || ls.UnpushedCommits != 0 {
			t.Errorf("CheckUncommittedWorkLocal = %+v, %v", ls, err)
		}
		if skew := pc.ClassifyIndexSkew(nil); len(skew) != 0 {
			t.Errorf("ClassifyIndexSkew(nil) = %v", skew)
		}
	})
}

func samePath(a, b string) bool { return resolved(a) == resolved(b) }

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
