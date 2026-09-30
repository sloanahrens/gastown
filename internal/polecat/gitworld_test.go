package polecat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/rig"
)

// world is the repositories one test's manager sees: a gitfake world, opened
// through the same gitOpener production code uses.
type world struct {
	*gitfake.Fake
}

func newWorld() *world { return &world{gitfake.New()} }

// opener opens the world's repositories the way the manager opens real ones.
func (w *world) opener() gitOpener {
	return gitOpener{
		open:    func(dir string) gitRepo { return w.OpenBranchRepo(dir) },
		openDir: func(gitDir, workDir string) gitRepo { return w.OpenWithDir(gitDir, workDir).(gitfake.BranchRepo) },
	}
}

// repo opens dir in the world.
func (w *world) repo(dir string) gitRepo { return w.OpenBranchRepo(dir) }

// checkout writes the commit HEAD names in the checkout at dir to disk, as
// git leaves a checkout after a commit it made itself.
func (w *world) checkout(t *testing.T, dir string) {
	t.Helper()
	if err := w.repo(dir).ResetHard("HEAD"); err != nil {
		t.Fatalf("checkout %s: %v", dir, err)
	}
}

// writeAndCommit writes files into the checkout at dir and commits them.
func (w *world) writeAndCommit(t *testing.T, dir, message string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return w.CommitWorktree(t, dir, message)
}

// buildCanonicalRigIn lays out the canonical rig at root in w: a mayor/rig
// repository with one commit on main, origin pointing at itself with an
// origin/main tracking ref, and a .beads redirect to mayor/rig/.beads.
func buildCanonicalRigIn(t *testing.T, w *world, root string) {
	t.Helper()
	buildRigIn(t, w, root, map[string]string{"README.md": "# Test\n"})
}

// buildRigIn is buildCanonicalRigIn with files as mayor/rig's first commit.
func buildRigIn(t *testing.T, w *world, root string, files map[string]string) {
	t.Helper()
	mayorRig := filepath.Join(root, "mayor", "rig")
	for _, dir := range []string{filepath.Join(root, ".beads"), filepath.Join(mayorRig, ".beads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.InitRepo(t, mayorRig)
	head := w.Commit(t, mayorRig, "main", "Initial commit", files)
	w.checkout(t, mayorRig)
	w.AddRemote(t, mayorRig, "origin", mayorRig)
	w.SetRef(t, mayorRig, "refs/remotes/origin/main", head)
}

// canonicalRig builds the canonical rig in a new world and returns its
// manager (with the fake agent bd), the mayor/rig path, the bd and the world.
func canonicalRig(t *testing.T) (*Manager, string, *fakeBd, *world) {
	t.Helper()
	w := newWorld()
	town := t.TempDir()
	root := filepath.Join(town, "rig")
	buildCanonicalRigIn(t, w, root)
	bd := newAgentBd(true)
	mgr := newTestManager(&rig.Rig{Name: "rig", Path: root}, w, nil, bd)
	return mgr, filepath.Join(root, "mayor", "rig"), bd, w
}

// canonicalWithPolecats is canonicalRig with the named polecats added
// through AddWithOptions and, with clean, their untracked files removed.
func canonicalWithPolecats(t *testing.T, clean bool, names ...string) (*Manager, string, *fakeBd, map[string]*Polecat, *world) {
	t.Helper()
	mgr, mayorRig, bd, w := canonicalRig(t)
	added := map[string]*Polecat{}
	for _, name := range names {
		p, err := mgr.AddWithOptions(name, AddOptions{})
		if err != nil {
			t.Fatalf("AddWithOptions(%s): %v", name, err)
		}
		if clean {
			if err := w.repo(p.ClonePath).CleanForce(); err != nil {
				t.Fatal(err)
			}
		}
		added[name] = p
	}
	return mgr, mayorRig, bd, added, w
}

// rev is git rev-parse ref in dir.
func (w *world) rev(t *testing.T, dir, ref string) string {
	t.Helper()
	id, err := w.repo(dir).Rev(ref)
	if err != nil {
		t.Fatalf("rev-parse %s in %s: %v", ref, dir, err)
	}
	return id
}

// branch is the branch checked out in dir, or "HEAD" when it is detached.
func (w *world) branch(t *testing.T, dir string) string {
	t.Helper()
	b, err := w.repo(dir).CurrentBranch()
	if err != nil {
		t.Fatalf("current branch in %s: %v", dir, err)
	}
	return b
}

// dirty reports whether dir has uncommitted changes.
func (w *world) dirty(t *testing.T, dir string) bool {
	t.Helper()
	st, err := w.repo(dir).CheckUncommittedWork()
	if err != nil {
		t.Fatalf("status in %s: %v", dir, err)
	}
	return st.HasUncommittedChanges
}

// switchTo checks out ref in dir.
func (w *world) switchTo(t *testing.T, dir, ref string) {
	t.Helper()
	if err := w.repo(dir).Checkout(ref); err != nil {
		t.Fatalf("checkout %s in %s: %v", ref, dir, err)
	}
}

// branchAtNewCommit creates branch in repo at a fresh commit parented on
// parent with parent's tree, without touching any working tree.
func (w *world) branchAtNewCommit(t *testing.T, repo, branch, parent, message string) string {
	t.Helper()
	w.SetRef(t, repo, "refs/heads/"+branch, parent)
	return w.Commit(t, repo, branch, message, nil)
}

// addRig is the rig the AddWithOptions tests build at a fresh root: files as
// mayor/rig's first commit, and beads readied by beadsFor.
func addRig(t *testing.T, beadsFor addBeads, files map[string]string) (*Manager, *world, string) {
	t.Helper()
	w := newWorld()
	root := t.TempDir()
	buildRigIn(t, w, root, files)
	mayorRig := filepath.Join(root, "mayor", "rig")
	bd := beadsFor(t, mayorRig, filepath.Join(mayorRig, ".beads"))
	return newTestManager(&rig.Rig{Name: "rig", Path: root}, w, nil, bd), w, mayorRig
}
