//go:build !windows

package util

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeGroup is a process group whose members die on the signals it is told to
// honour, standing in for /proc and kill(2), with a clock that only moves when
// the code under test sleeps.
type fakeGroup struct {
	mu       sync.Mutex
	members  map[int]bool
	diesOn   map[syscall.Signal]bool
	sent     []syscall.Signal
	groupErr error // what kill(-pgid, ...) answers
	listErr  error
	listFail int // list calls that succeed before listErr starts (<0: never fail)
	listN    int
	clock    time.Time
}

func newFakeGroup(diesOn ...syscall.Signal) *fakeGroup {
	g := &fakeGroup{members: map[int]bool{101: true, 102: true}, diesOn: map[syscall.Signal]bool{}, listFail: -1}
	for _, s := range diesOn {
		g.diesOn[s] = true
	}
	return g
}

func (g *fakeGroup) ops(grace time.Duration) groupOps {
	return groupOps{
		kill: func(pid int, sig syscall.Signal) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			if pid < 0 {
				return g.groupErr
			}
			return nil
		},
		list: func(int) ([]int, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.listN++
			if g.listFail >= 0 && g.listN > g.listFail {
				return nil, g.listErr
			}
			var out []int
			for pid, alive := range g.members {
				if alive {
					out = append(out, pid)
				}
			}
			return out, nil
		},
		now: func() time.Time {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.clock
		},
		sleep: func(d time.Duration) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.clock = g.clock.Add(d)
		},
		grace: grace,
		member: func(pid int, sig syscall.Signal) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.sent = append(g.sent, sig)
			if g.diesOn[sig] {
				g.members[pid] = false
			}
			return nil
		},
	}
}

func (g *fakeGroup) count(sig syscall.Signal) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, s := range g.sent {
		if s == sig {
			n++
		}
	}
	return n
}

var errKillRefused = errors.New("operation not permitted")

// The wiring: kill(-pgid) answering EPERM, as in an OpenShell sandbox, routes
// killGroup to the members, and each is signaled by pid.
func TestKillGroupFallsBackToMembersOnEPERM(t *testing.T) {
	t.Parallel()
	g := newFakeGroup(syscall.SIGTERM)
	g.groupErr = syscall.EPERM
	if err := g.ops(time.Second).killGroup(7); err != nil {
		t.Fatalf("killGroup = %v, want nil", err)
	}
	if got := g.count(syscall.SIGTERM); got != 2 {
		t.Errorf("SIGTERMs sent by pid = %d, want one per member", got)
	}
}

// Any other refusal is returned as it is: the member walk is for EPERM only.
func TestKillGroupOtherErrorsDoNotFallBack(t *testing.T) {
	t.Parallel()
	g := newFakeGroup(syscall.SIGTERM)
	g.groupErr = syscall.EINVAL
	if err := g.ops(time.Second).killGroup(7); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("killGroup = %v, want EINVAL", err)
	}
	if len(g.sent) != 0 {
		t.Errorf("members signaled after a non-EPERM error: %v", g.sent)
	}
}

// ESRCH on the first signal is a group that had already finished.
func TestKillGroupESRCHIsProcessDone(t *testing.T) {
	t.Parallel()
	g := newFakeGroup()
	g.groupErr = syscall.ESRCH
	if err := g.ops(time.Second).killGroup(7); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup = %v, want os.ErrProcessDone", err)
	}
}

// Members that exit on SIGTERM are not sent SIGKILL.
func TestKillMembersEndsMembersOnSIGTERM(t *testing.T) {
	t.Parallel()
	g := newFakeGroup(syscall.SIGTERM, syscall.SIGKILL)
	if err := g.ops(time.Second).killMembers(7, errKillRefused); err != nil {
		t.Fatalf("killMembers = %v, want nil", err)
	}
	if g.count(syscall.SIGKILL) != 0 || g.count(syscall.SIGTERM) != 2 {
		t.Errorf("signals = %v, want two SIGTERM and no SIGKILL", g.sent)
	}
}

// Members that ignore SIGTERM are killed once the grace has run out.
func TestKillMembersEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	g := newFakeGroup(syscall.SIGKILL)
	if err := g.ops(100*time.Millisecond).killMembers(7, errKillRefused); err != nil {
		t.Fatalf("killMembers = %v, want nil", err)
	}
	if g.count(syscall.SIGKILL) != 2 {
		t.Errorf("signals = %v, want one SIGKILL per surviving member", g.sent)
	}
}

// A kill that did not take is reported, not swallowed.
func TestKillMembersReportsSurvivorsOfSIGKILL(t *testing.T) {
	t.Parallel()
	g := newFakeGroup() // nothing dies
	err := g.ops(100*time.Millisecond).killMembers(7, errKillRefused)
	if err == nil {
		t.Fatal("killMembers = nil although every member survived SIGKILL")
	}
	if errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killMembers = %v: survivors must not read as a finished group", err)
	}
}

// Losing the listing mid-wait does not strand the group: the members last seen
// are killed.
func TestKillMembersListingLostMidWaitStillKills(t *testing.T) {
	t.Parallel()
	g := newFakeGroup(syscall.SIGKILL)
	g.listErr = errors.New("proc vanished")
	g.listFail = 1 // the first listing works, every later one fails
	if err := g.ops(time.Second).killMembers(7, errKillRefused); err != nil {
		t.Fatalf("killMembers = %v, want nil (survivors cannot be counted without a listing)", err)
	}
	if g.count(syscall.SIGKILL) != 2 {
		t.Errorf("signals = %v, want SIGKILL to both members last seen", g.sent)
	}
}

// An empty group is a group that had already finished, as for the group signal.
func TestKillMembersOfAnEmptyGroupIsProcessDone(t *testing.T) {
	t.Parallel()
	g := &fakeGroup{members: map[int]bool{}, listFail: -1}
	if err := g.ops(time.Millisecond).killMembers(7, errKillRefused); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killMembers of an empty group = %v, want os.ErrProcessDone", err)
	}
	if len(g.sent) != 0 {
		t.Errorf("signals sent to an empty group: %v", g.sent)
	}
}

// When the members cannot be listed there is nothing to do but report the
// refusal the group signal failed with.
func TestKillMembersWithoutAListingReturnsTheRefusal(t *testing.T) {
	t.Parallel()
	g := newFakeGroup()
	g.listErr = errors.New("no /proc")
	g.listFail = 0
	if err := g.ops(time.Millisecond).killMembers(7, errKillRefused); !errors.Is(err, errKillRefused) {
		t.Fatalf("killMembers without a listing = %v, want the original refusal", err)
	}
	if len(g.sent) != 0 {
		t.Errorf("signals sent without a listing: %v", g.sent)
	}
}
