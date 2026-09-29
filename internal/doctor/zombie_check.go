package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// zombieSessionLister abstracts the tmux operations this check needs, so
// tests can inject a failure without a real tmux server.
type zombieSessionLister interface {
	ListSessions() ([]string, error)
	IsAgentAliveChecked(session string) (bool, error)
	KillSessionWithProcesses(name string) error
}

// ZombieSessionCheck detects tmux sessions that are valid Gas Town sessions
// but have no Claude/node process running inside (zombies).
// These occur when Claude exits or crashes but the tmux session remains.
type ZombieSessionCheck struct {
	FixableCheck
	zombieSessions []string // Cached during Run for use in Fix

	listerForTest zombieSessionLister // nil → real tmux
}

// NewZombieSessionCheckWithLister creates a check with a custom lister (for testing).
func NewZombieSessionCheckWithLister(lister zombieSessionLister) *ZombieSessionCheck {
	c := NewZombieSessionCheck()
	c.listerForTest = lister
	return c
}

// NewZombieSessionCheck creates a new zombie session check.
func NewZombieSessionCheck() *ZombieSessionCheck {
	return &ZombieSessionCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "zombie-sessions",
				CheckDescription: "Detect tmux sessions with dead Claude processes",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run checks for zombie Gas Town sessions (tmux alive but Claude dead).
func (c *ZombieSessionCheck) Run(ctx *CheckContext) *CheckResult {
	var t zombieSessionLister = tmux.NewTmux()
	if c.listerForTest != nil {
		t = c.listerForTest
	}

	sessions, err := t.ListSessions()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not list tmux sessions",
			Details: []string{err.Error()},
		}
	}

	if len(sessions) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No tmux sessions found",
		}
	}

	// Check each Gas Town session for zombie status
	var zombies []string
	var unknown []string
	var healthyCount int

	for _, sess := range sessions {
		if sess == "" {
			continue
		}

		// Only check Gas Town sessions
		if !session.IsKnownSession(sess) {
			continue
		}

		// Skip crew sessions - they are human-managed and may intentionally
		// have no Claude running (e.g., between work assignments)
		if isCrewSession(sess) {
			continue
		}

		// Check if Claude is running in this session. A failed query is
		// UNKNOWN, never dead: under load `tmux show-environment` times out and
		// the old error-dropping check turned that into a zombie kill (G4-01).
		alive, err := t.IsAgentAliveChecked(sess)
		switch {
		case err != nil:
			unknown = append(unknown, fmt.Sprintf("Unknown: %s (liveness query failed: %v); not treated as a zombie", sess, err))
		case alive:
			healthyCount++
		default:
			zombies = append(zombies, sess)
		}
	}

	// Cache zombies for Fix
	c.zombieSessions = zombies

	if len(zombies) == 0 && len(unknown) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("unknown: could not verify %d session(s)", len(unknown)),
			Details: unknown,
		}
	}

	if len(zombies) == 0 {
		msg := "No zombie sessions found"
		if healthyCount > 0 {
			msg = fmt.Sprintf("All %d Gas Town sessions have running Claude processes", healthyCount)
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: msg,
		}
	}

	details := make([]string, 0, len(zombies)+len(unknown))
	for _, session := range zombies {
		details = append(details, fmt.Sprintf("Zombie: %s (tmux alive, Claude dead)", session))
	}
	details = append(details, unknown...)

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d zombie session(s)", len(zombies)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to kill zombie sessions",
	}
}

// Fix kills all zombie sessions (tmux sessions with no Claude running).
// Crew sessions are never auto-killed as they are human-managed.
func (c *ZombieSessionCheck) Fix(ctx *CheckContext) error {
	if len(c.zombieSessions) == 0 {
		return nil
	}

	var t zombieSessionLister = tmux.NewTmux()
	if c.listerForTest != nil {
		t = c.listerForTest
	}
	var lastErr error

	for _, sess := range c.zombieSessions {
		// SAFEGUARD: Never auto-kill crew sessions (double-check)
		if isCrewSession(sess) {
			continue
		}

		// TOCTOU guard: re-verify Claude is still dead in this session.
		// Between Run() identifying zombies and Fix() killing them,
		// a Claude process may have started (e.g., session was restarted).
		// Only a confirmed "not alive" answer proceeds; a query error is
		// UNKNOWN and the session is left alone (G4-01).
		if alive, err := t.IsAgentAliveChecked(sess); err != nil || alive {
			continue
		}

		// Log pre-death event for audit trail
		_ = events.LogFeed(events.TypeSessionDeath, sess,
			events.SessionDeathPayload(sess, "unknown", "zombie cleanup", "gt doctor"))

		// Use KillSessionWithProcesses to ensure all descendant processes are killed.
		if err := t.KillSessionWithProcesses(sess); err != nil {
			lastErr = err
		}
	}

	return lastErr
}
