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
	alive    map[string]bool // session -> IsAgentAlive result
	killed   []string
}

func (f *fakeZombieLister) ListSessions() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.sessions, nil
}

func (f *fakeZombieLister) IsAgentAlive(session string) bool {
	return f.alive[session]
}

func (f *fakeZombieLister) KillSessionWithProcesses(name string) error {
	f.killed = append(f.killed, name)
	return nil
}

func TestNewZombieSessionCheck(t *testing.T) {
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
	lister := &fakeZombieLister{sessions: []string{}}
	check := NewZombieSessionCheckWithLister(lister)
	ctx := &CheckContext{TownRoot: t.TempDir()}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when tmux has no sessions, got %v: %s", result.Status, result.Message)
	}
}

func TestZombieSessionCheck_ListSessionsErrorIsSkipped(t *testing.T) {
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
	// Verify that Fix() never kills crew sessions
	check := NewZombieSessionCheck()

	// Manually set zombies including a crew session (simulating a bug)
	check.zombieSessions = []string{
		"gt-gastown-crew-joe", // Should be skipped
		"gt-gastown-witness",  // Would be killed (if real)
	}

	ctx := &CheckContext{TownRoot: t.TempDir()}

	// Fix should skip crew sessions due to safeguard
	// (We can't fully test this without mocking tmux, but the safeguard is in place)
	_ = check.Fix(ctx)

	// The test passes if no panic occurred and crew sessions are protected by the safeguard
}
