package daemon

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/land"
)

// gitLockProbe is the daemon's git seam in the seat's worktree: it records
// whether the lander's git lock was held when the refresh fetched.
type gitLockProbe struct {
	daemonGit
	lander  *land.Lander
	fetched bool
	held    bool
}

func (p *gitLockProbe) RefreshRemoteDefaultBranch(remote string) error {
	p.fetched = true
	p.held = p.lander.GitLockHeld()
	return p.daemonGit.RefreshRemoteDefaultBranch(remote)
}

// TestLandingGitLock pins the fix (gt-vwfzey): the author seat's worktree is
// linked to the rig's shared repository, so its refresh writes the same
// remote-tracking ref the landing's target fetch writes, and a deferred
// landing runs it beside a landing's fetch. Without the worker's wrapper the
// two park on 'cannot lock ref'; with it the refresh fetches under the lock.
func TestLandingGitLock(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	promoteRigConfig(t, townRoot, "gastown", filepath.Join(t.TempDir(), "promote-gastown.key"))
	writeRigsJSON(t, townRoot, []string{"gastown"})
	d := newForgejoPromoteDaemon(t, townRoot, "")

	w, err := d.newRigLandingWorker("gastown")
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	lander, ok := w.Lander.(*land.Lander)
	if !ok {
		t.Fatalf("worker Lander = %T, want *land.Lander", w.Lander)
	}
	// A GitLockHeld that answered true for a lock nothing holds would make
	// the assertion below pass without the wrapper.
	if lander.GitLockHeld() {
		t.Fatal("the lander's git lock is held before any git call")
	}

	// The seat the worker refreshes, cloned in a fake world of its own: the
	// refresh is the only git call this test drives.
	const seat = "furiosa"
	seatDir := filepath.Join(townRoot, "gastown", "polecats", seat, "gastown")
	f := gitfake.New()
	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	f.Commit(t, origin, "main", "initial", map[string]string{"README.md": "# Test\n"})
	f.Clone(t, origin, seatDir)

	probe := &gitLockProbe{lander: lander}
	d.openGitFn = func(dir string) daemonGit {
		probe.daemonGit = f.Open(dir).(daemonGit)
		return probe
	}

	if err := w.RefreshAuthorSeat(seat); err != nil {
		t.Fatalf("RefreshAuthorSeat: %v", err)
	}
	if !probe.fetched {
		t.Fatal("the seat refresh never fetched; the probe had no git call to judge")
	}
	if !probe.held {
		t.Fatal("the seat refresh fetched without the lander's git lock held; a landing's target fetch could write the same remote-tracking ref at once and park on 'cannot lock ref'")
	}
}
