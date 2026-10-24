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
// honour, standing in for /proc and kill(2).
type fakeGroup struct {
	mu      sync.Mutex
	members map[int]bool
	diesOn  map[syscall.Signal]bool
	sent    []string
}

func newFakeGroup(diesOn ...syscall.Signal) *fakeGroup {
	g := &fakeGroup{members: map[int]bool{101: true, 102: true}, diesOn: map[syscall.Signal]bool{}}
	for _, s := range diesOn {
		g.diesOn[s] = true
	}
	return g
}

func (g *fakeGroup) list(int) ([]int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []int
	for pid, alive := range g.members {
		if alive {
			out = append(out, pid)
		}
	}
	return out, nil
}

func (g *fakeGroup) signal(pid int, sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, sig.String())
	if g.diesOn[sig] {
		g.members[pid] = false
	}
	return nil
}

var errKillRefused = errors.New("operation not permitted")

// A sandbox that refuses kill(-pgid) with EPERM still gets its group ended: each
// member is signaled by pid, SIGTERM first.
func TestKillGroupMembersEndsMembersOnSIGTERM(t *testing.T) {
	t.Parallel()
	g := newFakeGroup(syscall.SIGTERM, syscall.SIGKILL)
	if err := killGroupMembers(7, errKillRefused, g.list, g.signal, 50*time.Millisecond); err != nil {
		t.Fatalf("killGroupMembers = %v, want nil", err)
	}
	for _, s := range g.sent {
		if s == syscall.SIGKILL.String() {
			t.Errorf("SIGKILL sent although every member exited on SIGTERM: %v", g.sent)
		}
	}
	if len(g.sent) != 2 {
		t.Errorf("signals sent = %v, want one SIGTERM per member", g.sent)
	}
}

// Members that ignore SIGTERM are killed once the grace has run out.
func TestKillGroupMembersEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	g := newFakeGroup(syscall.SIGKILL)
	if err := killGroupMembers(7, errKillRefused, g.list, g.signal, 10*time.Millisecond); err != nil {
		t.Fatalf("killGroupMembers = %v, want nil", err)
	}
	var kills int
	for _, s := range g.sent {
		if s == syscall.SIGKILL.String() {
			kills++
		}
	}
	if kills != 2 {
		t.Errorf("SIGKILLs sent = %d (%v), want one per surviving member", kills, g.sent)
	}
}

// An empty group is a group that had already finished, as for the group signal.
func TestKillGroupMembersOfAnEmptyGroupIsProcessDone(t *testing.T) {
	t.Parallel()
	g := &fakeGroup{members: map[int]bool{}}
	if err := killGroupMembers(7, errKillRefused, g.list, g.signal, time.Millisecond); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroupMembers of an empty group = %v, want os.ErrProcessDone", err)
	}
	if len(g.sent) != 0 {
		t.Errorf("signals sent to an empty group: %v", g.sent)
	}
}

// When the members cannot be listed there is nothing to do but report the
// refusal the group signal failed with.
func TestKillGroupMembersWithoutAListingReturnsTheRefusal(t *testing.T) {
	t.Parallel()
	g := newFakeGroup()
	unlistable := func(int) ([]int, error) { return nil, errors.New("no /proc") }
	if err := killGroupMembers(7, errKillRefused, unlistable, g.signal, time.Millisecond); !errors.Is(err, errKillRefused) {
		t.Fatalf("killGroupMembers without a listing = %v, want the original refusal", err)
	}
	if len(g.sent) != 0 {
		t.Errorf("signals sent without a listing: %v", g.sent)
	}
}
