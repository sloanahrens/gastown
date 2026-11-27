package daemon

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
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

// TestLandingGitLockAuthorSeatRefresh pins the fix (gt-vwfzey): the author
// seat's worktree is linked to the rig's shared repository, so its refresh
// writes the same remote-tracking ref the landing's target fetch writes, and a
// deferred landing runs it beside a landing's fetch. Without the worker's
// wrapper the two park on 'cannot lock ref'; with it the refresh fetches under
// the lock.
func TestLandingGitLockAuthorSeatRefresh(t *testing.T) {
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

// writeProbe is the remote git surface the table drives: it records each ref
// write and the ones made without the lander's git lock held. The embedded
// interface panics on a call no row drives.
type writeProbe struct {
	landingRemoteGit
	lander *land.Lander
	calls  []string
	unheld []string
}

func (p *writeProbe) record(call string) error {
	p.calls = append(p.calls, call)
	if !p.lander.GitLockHeld() {
		p.unheld = append(p.unheld, call)
	}
	return nil
}

func (p *writeProbe) FetchRefspecWithTimeout(string, string, time.Duration) error {
	return p.record("fetch")
}

func (p *writeProbe) DeleteRemoteBranchIfAt(string, string, string) error {
	return p.record("delete remote")
}

func (p *writeProbe) DeleteLocalBranchIfAt(string, string) error { return p.record("delete local") }

func (p *writeProbe) RefExists(string) (bool, error)                   { return true, nil }
func (p *writeProbe) IsAncestor(string, string) (bool, error)          { return true, nil }
func (p *writeProbe) CommitLandedOnTarget(string, string, string) bool { return true }
func (p *writeProbe) CurrentBranch() (string, error)                   { return "main", nil }
func (p *writeProbe) WorktreeList() ([]git.Worktree, error)            { return nil, nil }

// TestLandingGitLock pins gt-5n49q4: every ref write the landing worker's
// remote handle makes runs while its Lander holds the git lock, so the
// bookkeeping jobs running beside a landing cannot park it on 'cannot lock
// ref'. Each row drives one method of the built worker's Remote.
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
	lander, isLander := w.Lander.(*land.Lander)
	remote, isRemote := w.Remote.(gitRemote)
	if !isLander || !isRemote {
		t.Fatalf("worker Lander = %T, Remote = %T, want *land.Lander and the daemon's gitRemote", w.Lander, w.Remote)
	}
	const head = "0123456789abcdef0123456789abcdef01234567"

	tests := []struct {
		name   string
		drive  func(gitRemote) error
		writes []string
	}{
		{"Contains", func(r gitRemote) error { _, err := r.Contains("main", head); return err }, []string{"fetch"}},
		{"BranchLandedOn", func(r gitRemote) error { _, err := r.BranchLandedOn("main", "polecat/x", head); return err }, []string{"fetch", "fetch"}},
		{"DeleteRemoteBranchIfAt", func(r gitRemote) error { return r.DeleteRemoteBranchIfAt("polecat/x", head) }, []string{"delete remote"}},
		{"DeleteLocalBranch", func(r gitRemote) error { _, err := r.DeleteLocalBranch("polecat/x", head); return err }, []string{"delete local"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &writeProbe{lander: lander}
			r := remote
			r.g = probe
			if err := tt.drive(r); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if !slices.Equal(probe.calls, tt.writes) || len(probe.unheld) > 0 {
				t.Errorf("writes = %v (unheld %v), want %v with the lander's git lock held", probe.calls, probe.unheld, tt.writes)
			}
		})
	}
}
