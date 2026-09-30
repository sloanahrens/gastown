package doctor

import (
	"errors"
	"strings"
	"testing"
)

// fakeZombieLister implements zombieSessionLister for testing.
type fakeZombieLister struct {
	sessions []string
	listErr  error
	alive    map[string]bool  // session -> IsAgentAliveChecked result
	aliveErr map[string]error // session -> IsAgentAliveChecked error
	killed   []string
}

func (f *fakeZombieLister) ListSessions() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.sessions, nil
}

func (f *fakeZombieLister) IsAgentAliveChecked(session string) (bool, error) {
	if err := f.aliveErr[session]; err != nil {
		return false, err
	}
	return f.alive[session], nil
}

func (f *fakeZombieLister) KillSessionWithProcesses(name string) error {
	f.killed = append(f.killed, name)
	return nil
}

func TestNewZombieSessionCheck(t *testing.T) {
	t.Parallel()
	check := NewZombieSessionCheck()

	if check.Name() != "zombie-sessions" {
		t.Errorf("expected name 'zombie-sessions', got %q", check.Name())
	}

	if check.Description() != "Detect tmux sessions with dead Claude processes" {
		t.Errorf("expected description 'Detect tmux sessions with dead Claude processes', got %q", check.Description())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}

	if check.Category() != CategoryCleanup {
		t.Errorf("expected category %q, got %q", CategoryCleanup, check.Category())
	}
}

func TestZombieSessionCheck_Run_NoSessions(t *testing.T) {
	t.Parallel()
	lister := &fakeZombieLister{sessions: []string{}}
	check := NewZombieSessionCheckWithLister(lister)
	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when tmux has no sessions, got %v: %s", result.Status, result.Message)
	}
}

func TestZombieSessionCheck_ListSessionsErrorIsSkipped(t *testing.T) {
	t.Parallel()
	lister := &fakeZombieLister{listErr: errors.New("no server running")}
	check := NewZombieSessionCheckWithLister(lister)
	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}

	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped — could not list sessions", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "no server running") {
		t.Errorf("Details = %v, want the underlying error", result.Details)
	}
}

func TestZombieSessionCheck_SkipsCrewSessions(t *testing.T) {
	t.Parallel()
	// A crew session with no Claude is human-managed, not a zombie; a dead
	// witness beside it is.
	lister := &fakeZombieLister{sessions: []string{"gt-crew-joe", "gt-witness"}}
	check := NewZombieSessionCheckWithLister(lister)

	result := check.Run(&CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for the dead witness: %s", result.Status, result.Message)
	}
	for _, detail := range result.Details {
		if strings.Contains(detail, "crew") {
			t.Errorf("crew session should not be in zombie list: %s", detail)
		}
	}
	if len(check.zombieSessions) != 1 || check.zombieSessions[0] != "gt-witness" {
		t.Errorf("zombies = %v, want only gt-witness", check.zombieSessions)
	}
}

func TestZombieSessionCheck_FixProtectsCrewSessions(t *testing.T) {
	t.Parallel()
	// Fix never kills a crew session, even one Run wrongly listed.
	lister := &fakeZombieLister{}
	check := NewZombieSessionCheckWithLister(lister)
	check.zombieSessions = []string{"gt-crew-joe", "gt-witness"}

	_ = check.Fix(&CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()})

	if len(lister.killed) != 1 || lister.killed[0] != "gt-witness" {
		t.Fatalf("killed = %v, want only gt-witness (never the crew session)", lister.killed)
	}
}

// TestZombieSessionCheck_LivenessErrorIsNotAZombie is gt-fcxe9.1: a liveness
// query that fails (tmux show-environment timing out under load) is UNKNOWN,
// not dead. Run must not list the session as a zombie and Fix must not kill it.
func TestZombieSessionCheck_LivenessErrorIsNotAZombie(t *testing.T) {
	t.Parallel()
	lister := &fakeZombieLister{
		sessions: []string{"hq-mayor", "hq-overseer"},
		alive:    map[string]bool{"hq-overseer": false},
		aliveErr: map[string]error{"hq-mayor": errors.New("tmux show-environment: timed out")},
	}
	check := NewZombieSessionCheckWithLister(lister)
	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}

	result := check.Run(ctx)
	for _, d := range result.Details {
		if strings.Contains(d, "Zombie: hq-mayor") {
			t.Fatalf("session with an unknown liveness answer listed as zombie: %v", result.Details)
		}
	}
	foundUnknown := false
	for _, d := range result.Details {
		if strings.Contains(d, "hq-mayor") && strings.Contains(strings.ToLower(d), "unknown") {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Errorf("expected the unknown session reported as unknown, got %v", result.Details)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, k := range lister.killed {
		if k == "hq-mayor" {
			t.Fatal("Fix killed a session whose liveness query failed")
		}
	}
}

// TestZombieSessionCheck_FixRecheckErrorSkipsKill: the TOCTOU re-check in Fix
// must also treat a query error as unknown and leave the session alone.
func TestZombieSessionCheck_FixRecheckErrorSkipsKill(t *testing.T) {
	t.Parallel()
	lister := &fakeZombieLister{
		sessions: []string{"hq-mayor"},
		alive:    map[string]bool{"hq-mayor": false},
	}
	check := NewZombieSessionCheckWithLister(lister)
	ctx := &CheckContext{TownRoot: t.TempDir(), sessionPrefixes: testPrefixRegistry()}
	_ = check.Run(ctx)
	lister.aliveErr = map[string]error{"hq-mayor": errors.New("tmux: server busy")}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(lister.killed) != 0 {
		t.Fatalf("Fix killed %v after a failed re-check", lister.killed)
	}
}
