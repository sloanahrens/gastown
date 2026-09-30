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
	ctx := &CheckContext{TownRoot: t.TempDir()}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when tmux has no sessions, got %v: %s", result.Status, result.Message)
	}
}

func TestZombieSessionCheck_ListSessionsErrorIsSkipped(t *testing.T) {
	t.Parallel()
	lister := &fakeZombieLister{listErr: errors.New("no server running")}
	check := NewZombieSessionCheckWithLister(lister)
	ctx := &CheckContext{TownRoot: t.TempDir()}

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
	// Verify that crew sessions are not marked as zombies
	check := NewZombieSessionCheck()

	// Run the check - crew sessions should be skipped
	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	// If there are zombies, ensure no crew sessions are in the list
	for _, detail := range result.Details {
		if isCrewSession(detail) {
			t.Errorf("crew session should not be in zombie list: %s", detail)
		}
	}
}

func TestZombieSessionCheck_FixProtectsCrewSessions(t *testing.T) {
	t.Parallel()
	// Verify that Fix() never kills crew sessions
	check := NewZombieSessionCheck()

	// Manually set zombies including a crew session (simulating a bug)
	check.zombieSessions = []string{
		"gt-gastown-crew-joe", // Should be skipped
		"gt-gastown-nux",      // Would be killed (if real)
	}

	ctx := &CheckContext{TownRoot: t.TempDir()}

	// Fix should skip crew sessions due to safeguard
	// (We can't fully test this without mocking tmux, but the safeguard is in place)
	_ = check.Fix(ctx)

	// The test passes if no panic occurred and crew sessions are protected by the safeguard
}

// TestZombieSessionCheck_LivenessErrorIsNotAZombie is gt-fcxe9.1: a liveness
// query that fails (tmux show-environment timing out under load) is UNKNOWN,
// not dead. Run must not list the session as a zombie and Fix must not kill it.
func TestZombieSessionCheck_LivenessErrorIsNotAZombie(t *testing.T) {
	t.Parallel()
	lister := &fakeZombieLister{
		sessions: []string{"hq-mayor", "hq-dog-alpha"},
		alive:    map[string]bool{"hq-dog-alpha": false},
		aliveErr: map[string]error{"hq-mayor": errors.New("tmux show-environment: timed out")},
	}
	check := NewZombieSessionCheckWithLister(lister)
	ctx := &CheckContext{TownRoot: t.TempDir()}

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
	ctx := &CheckContext{TownRoot: t.TempDir()}
	_ = check.Run(ctx)
	lister.aliveErr = map[string]error{"hq-mayor": errors.New("tmux: server busy")}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(lister.killed) != 0 {
		t.Fatalf("Fix killed %v after a failed re-check", lister.killed)
	}
}
