package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

const (
	livenessPolecat = "ember"
	livenessAgent   = "deepseek-flash-sandbox"
	livenessBeadID  = "gt-gastown-polecat-ember"
	livenessSession = "gt-gastown-p-ember"
)

// livenessStore is the bead database the cap's seat question reads through.
// It counts each id's Show, so a test can tell a read the sling's memo served
// from one the store paid.
type livenessStore struct {
	*beadsfake.Fake
	mu    sync.Mutex
	shows map[string]int
}

func newLivenessStore() *livenessStore {
	return &livenessStore{Fake: beadsfake.New(), shows: map[string]int{}}
}

func (s *livenessStore) Show(id string) (*beads.Issue, error) {
	s.mu.Lock()
	s.shows[id]++
	s.mu.Unlock()
	return s.Fake.Show(id)
}

func (s *livenessStore) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shows[id]
}

// RecordReassignment and FindMRForBranchAny complete polecat.Store; the seat
// question reads only Show.
func (s *livenessStore) RecordReassignment(string, string, string, string, []string) error {
	return nil
}

func (s *livenessStore) FindMRForBranchAny(string) (*beads.Issue, error) { return nil, nil }

// livenessManager is a polecat Manager over store, under a temp rig, with no
// tmux or git: the seat question reads the store alone.
func livenessManager(t *testing.T, store polecat.Store) *polecat.Manager {
	t.Helper()
	rigPath := filepath.Join(t.TempDir(), "gastown")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return polecat.NewManagerWithStore(&rig.Rig{Name: "gastown", Path: rigPath}, nil, nil, townRegistry(), store)
}

// livenessSeatStore returns a store holding the seat's agent bead, recording
// agent in its agent field.
func livenessSeatStore(t *testing.T, agent string) *livenessStore {
	t.Helper()
	store := newLivenessStore()
	if _, err := store.Create(beads.CreateOptions{
		ID: livenessBeadID, Title: livenessBeadID, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(livenessBeadID, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", Agent: agent,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// livenessCounter builds the cap's seat question over mgr and the given probe.
func livenessCounter(mgr *polecat.Manager, probe sandboxSeatProbe) liveSeatCounter {
	return newLiveSeatCounter(mgr,
		func(string) string { return livenessBeadID },
		func(string) string { return livenessSession },
		probe)
}

// livenessHostProbe answers as the probe does for a tmux seat.
func livenessHostProbe(string, string) (polecat.LiveGitState, bool) {
	return polecat.LiveGitState{}, false
}

// livenessSandboxProbe answers as the probe does for a sandbox seat the gateway
// answered exists for: true is a running sandbox, false a gone one, and an
// error a gateway that did not answer.
func livenessSandboxProbe(exists bool, existsErr error) sandboxSeatProbe {
	return func(string, string) (polecat.LiveGitState, bool) {
		return polecat.SandboxSeatGitState("", exists, existsErr, livenessSession), true
	}
}

// TestPolecatSeatLiveness: the cap counts a polecat directory unless every read
// agrees it is a sandbox seat whose sandbox is gone. Each row is one fact.
func TestPolecatSeatLiveness(t *testing.T) {
	t.Parallel()
	unreadable := newLivenessStore()
	unreadable.FailWith("show", errors.New("bd unavailable"))

	tests := []struct {
		name  string
		mgr   *polecat.Manager
		probe sandboxSeatProbe
		want  bool
	}{
		{"host seat counts", livenessManager(t, livenessSeatStore(t, "deepseek-flash")), livenessHostProbe, true},
		{"sandbox seat whose sandbox exists counts", livenessManager(t, livenessSeatStore(t, livenessAgent)), livenessSandboxProbe(true, nil), true},
		{"sandbox seat whose sandbox is gone does not count", livenessManager(t, livenessSeatStore(t, livenessAgent)), livenessSandboxProbe(false, nil), false},
		{"gateway error counts", livenessManager(t, livenessSeatStore(t, livenessAgent)), livenessSandboxProbe(false, errors.New("gateway down")), true},
		{"unreadable agent bead counts", livenessManager(t, unreadable), livenessSandboxProbe(false, nil), true},
		{"no agent bead counts", livenessManager(t, newLivenessStore()), livenessSandboxProbe(false, nil), true},
		{"no probe counts", livenessManager(t, livenessSeatStore(t, livenessAgent)), nil, true},
		{"no manager counts", nil, livenessSandboxProbe(false, nil), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := livenessCounter(tt.mgr, tt.probe)(livenessPolecat); got != tt.want {
				t.Errorf("live seat = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPolecatSeatLivenessAsksForTheSeat: the probe is asked about the agent the
// bead recorded and the seat's session, which is also its sandbox's name.
func TestPolecatSeatLivenessAsksForTheSeat(t *testing.T) {
	t.Parallel()
	var gotAgent, gotSession string
	probe := func(agent, sessionName string) (polecat.LiveGitState, bool) {
		gotAgent, gotSession = agent, sessionName
		return polecat.SandboxSeatGitState("", false, nil, sessionName), true
	}

	livenessCounter(livenessManager(t, livenessSeatStore(t, livenessAgent)), probe)(livenessPolecat)

	if gotAgent != livenessAgent {
		t.Errorf("probe agent = %q, want the bead's %q", gotAgent, livenessAgent)
	}
	if gotSession != livenessSession {
		t.Errorf("probe session = %q, want %q", gotSession, livenessSession)
	}
}

// TestPolecatSeatLivenessReadsThroughTheSlingMemo: the cap asks the manager for
// the seat's agent bead, so the sling's open memo serves a bead the seat
// listing already read. The cap asks twice; a read that bypassed the memo
// would pay the store a second time for the same seat, and a read that
// bypassed the manager would not pay the store at all (gt-atlmw2).
func TestPolecatSeatLivenessReadsThroughTheSlingMemo(t *testing.T) {
	t.Parallel()
	store := livenessSeatStore(t, livenessAgent)
	mgr := livenessManager(t, store)
	counter := livenessCounter(mgr, livenessSandboxProbe(false, nil))

	end := mgr.ShareAgentBeadReads()
	defer end()

	for i := 0; i < 2; i++ {
		if counter(livenessPolecat) {
			t.Fatalf("question %d: sandbox-gone seat counted as live", i+1)
		}
	}
	if got := store.count(livenessBeadID); got != 1 {
		t.Errorf("agent bead read from the store %d times, want 1: the cap read past the sling memo", got)
	}
}

// TestRealIdleReuseEnvSeatReadSharesTheSlingMemo drives the production wiring
// function, not the counter helper: the seat question realIdleReuseEnv builds
// for the cap must ask the sling's polecat manager, so a bead the seat listing
// already read is not read from the store again (gt-atlmw2). A read that
// bypassed the manager's memo pays the store a second time; one that bypassed
// the manager does not pay this store at all.
func TestRealIdleReuseEnvSeatReadSharesTheSlingMemo(t *testing.T) {
	t.Parallel()
	rigPath := filepath.Join(t.TempDir(), "gastown")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rig.Rig{Name: "gastown", Path: rigPath}
	// A host agent: the temp town has no seat map, so the probe answers "host"
	// without asking a gateway.
	store := livenessSeatStore(t, "deepseek-flash")
	mgr := polecat.NewManagerWithStore(r, nil, nil, townRegistry(), store)
	env := realIdleReuseEnv(slingStores{}, nil, r, filepath.Dir(rigPath), "gastown", func(string) {}, mgr)

	end := mgr.ShareAgentBeadReads()
	defer end()

	// The cap asks twice about one seat: the first question reads the store,
	// the second must be answered from the memo.
	for i := 0; i < 2; i++ {
		if !env.seatLive(livenessPolecat) {
			t.Fatalf("question %d: host seat not counted", i+1)
		}
	}
	if got := store.count(livenessBeadID); got != 1 {
		t.Errorf("agent bead read from the store %d times, want 1: realIdleReuseEnv does not ask the sling's manager through its memo", got)
	}
}
