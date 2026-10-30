package polecat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/rig"
)

// The pool lock is the count-and-claim every spawn of a rig takes, so what runs
// while it is held is what another sling waits on. The count is the polecats
// directory: a rig of a dozen seats that loaded each polecat's bead state for
// it would spend every other spawn's wait on reads reconcile never uses
// (gt-vaivp).
func TestReconcileUnderThePoolLockReadsOnlyThePolecatsDirectory(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	fullPolecat(t, rigPath, "myrig", "furiosa")
	r := &rig.Rig{Name: "myrig", Path: rigPath}
	db := newNoDatabaseDB()
	m := newTestManager(r, newWorld(), nil, db)

	before := db.readCount()
	m.ReconcilePool()

	if got := db.readCount() - before; got != 0 {
		t.Errorf("reconcile read %d bead(s) under the pool lock; its count is the polecats directory", got)
	}
	if name, err := m.namePool.Allocate(); err != nil {
		t.Fatalf("Allocate: %v", err)
	} else if name == "furiosa" {
		t.Error("furiosa's directory did not count as in use: reconcile allocated a name a polecat already holds")
	}
}

// The worktree prune is git work, and it runs once the pool lock is back: held
// across it, the prune would make every other spawn of the rig wait on git
// (gt-vaivp).
func TestTheWorktreePruneRunsAfterThePoolLockIsReleased(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rig.Rig{Name: "myrig", Path: rigPath}
	w := newWorld()
	m := newTestManager(r, w, nil, newNoDatabaseDB())

	lockPath := filepath.Join(rigPath, ".runtime", "locks", "polecat-pool.lock")
	pruned := 0
	free := false
	m.gits = gitOpener{open: func(dir string) gitRepo {
		return pruneProbe{gitRepo: w.OpenBranchRepo(dir), onPrune: func() {
			pruned++
			free = poolLockIsFree(t, lockPath)
		}}
	}}

	m.ReconcilePool()

	if pruned != 1 {
		t.Fatalf("the worktree prune ran %d time(s), want once", pruned)
	}
	if !free {
		t.Error("the worktree prune ran while the pool lock was held; every other spawn waits on that git call")
	}
}

// pruneProbe is a repository whose prune runs onPrune, so a test can see what
// the pool lock is doing at that moment.
type pruneProbe struct {
	gitRepo
	onPrune func()
}

func (p pruneProbe) WorktreePrune() error {
	p.onPrune()
	return p.gitRepo.WorktreePrune()
}

// poolLockIsFree reports whether the rig's pool lock can be taken right now.
func poolLockIsFree(t *testing.T, lockPath string) bool {
	t.Helper()
	l := flock.New(lockPath)
	ok, err := l.TryLock()
	if err != nil {
		t.Fatalf("probing the pool lock: %v", err)
	}
	if ok {
		_ = l.Unlock()
	}
	return ok
}

// fullPolecat lays out a polecat of rig "myrig" as a running spawn leaves it:
// its directory, and the clone directory with the .git partial-spawn reconcile
// looks for. Without them reconcile would try to reclaim the directory, which
// is a different path.
func fullPolecat(t *testing.T, rigPath, rigName, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats", name, rigName, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}
