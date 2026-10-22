package doctor

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/tmux"
)

// identitySessionLister is the tmux session listing the check cross-references
// locks against.
type identitySessionLister interface {
	ListSessions() ([]string, error)
	ListSessionIDs() (map[string]string, error)
}

// IdentityCollisionCheck checks for agent identity collisions and stale locks.
type IdentityCollisionCheck struct {
	BaseCheck
	lister identitySessionLister // nil means use real tmux
}

// NewIdentityCollisionCheck creates a new identity collision check.
func NewIdentityCollisionCheck() *IdentityCollisionCheck {
	return &IdentityCollisionCheck{
		BaseCheck: BaseCheck{
			CheckName:        "identity-collision",
			CheckDescription: "Check for agent identity collisions and stale locks",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

func (c *IdentityCollisionCheck) CanFix() bool {
	return true // Can fix stale locks
}

func (c *IdentityCollisionCheck) Run(ctx *CheckContext) *CheckResult {
	// Find all locks
	locks, err := lock.FindAllLocks(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("could not scan for locks: %v", err),
		}
	}

	if len(locks) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "no worker locks found",
		}
	}

	// Get active tmux sessions for cross-reference. A failed listing is kept,
	// not returned: only locks that name a session need it.
	lister := c.lister
	if lister == nil {
		lister = tmux.NewTmux()
	}
	sessionSet, listErr := identitySessionSet(lister)

	var staleLocks []string
	var orphanedLocks []string
	var healthyLocks int
	var unknownLocks int

	for workerDir, info := range locks {
		// Only a lock that names a session needs the listing; one without
		// is decided by its PID alone.
		if info.SessionID != "" && listErr != nil {
			unknownLocks++
			continue
		}

		// First check if the session exists in tmux - that's the real indicator
		// of whether the worker is alive. The PID in the lock is the spawning
		// process, which may have exited even though Claude is still running.
		sessionExists := info.SessionID != "" && sessionSet[info.SessionID]

		if info.IsStale() {
			// PID is dead - but is the session still alive?
			if sessionExists {
				// Session exists, so the worker is alive despite dead PID.
				// This is normal - the spawner exits after launching Claude.
				healthyLocks++
				continue
			}
			// Both the writer gone AND session gone = truly stale
			staleLocks = append(staleLocks,
				fmt.Sprintf("%s (%s)", workerDir, info.StaleReason()))
			continue
		}

		// PID is alive - check if session exists
		if info.SessionID != "" && !sessionSet[info.SessionID] {
			// Lock has session ID but session doesn't exist
			// This could be a collision or orphan
			orphanedLocks = append(orphanedLocks,
				fmt.Sprintf("%s (PID %d, missing session %s)", workerDir, info.PID, info.SessionID))
			continue
		}

		healthyLocks++
	}

	// Build result
	if len(staleLocks) == 0 && len(orphanedLocks) == 0 {
		if unknownLocks > 0 {
			return identityListingUnknown(c.Name(), listErr)
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("%d worker lock(s), all healthy", healthyLocks),
		}
	}

	result := &CheckResult{
		Name: c.Name(),
	}

	if len(staleLocks) > 0 {
		result.Status = StatusWarning
		result.Message = fmt.Sprintf("%d stale lock(s) found", len(staleLocks))
		result.Details = append(result.Details, "Stale locks (the writer is gone):")
		for _, s := range staleLocks {
			result.Details = append(result.Details, "  "+s)
		}
		result.FixHint = "Run 'gt doctor fix identity-collision' or 'gt agents fix' to clean up"
	}

	if len(orphanedLocks) > 0 {
		if result.Status != StatusWarning {
			result.Status = StatusWarning
		}
		if result.Message != "" {
			result.Message += ", "
		}
		result.Message += fmt.Sprintf("%d orphaned lock(s)", len(orphanedLocks))
		result.Details = append(result.Details, "Orphaned locks (missing sessions):")
		for _, s := range orphanedLocks {
			result.Details = append(result.Details, "  "+s)
		}
		if !strings.Contains(result.FixHint, "doctor") {
			result.FixHint = "Run 'gt doctor fix identity-collision' to clean up stale locks"
		}
	}

	if unknownLocks > 0 {
		result.Message += fmt.Sprintf(", %d lock(s) state unknown: could not list tmux sessions: %v",
			unknownLocks, listErr)
	}

	return result
}

// identitySessionSet returns the tmux session names and IDs that locks are
// cross-referenced against. A failed listing leaves the set incomplete, so the
// caller must not call a lock stale or orphaned from it.
func identitySessionSet(lister identitySessionLister) (map[string]bool, error) {
	sessionSet := make(map[string]bool)

	sessions, err := lister.ListSessions() // Returns session names
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		sessionSet[s] = true
	}

	// Also get session IDs to handle locks that store ID instead of name
	// Lock files may contain session_id in formats like "%55" or "$55"
	sessionIDs, err := lister.ListSessionIDs() // Returns map[name]id
	if err != nil {
		return nil, err
	}
	for _, id := range sessionIDs {
		sessionSet[id] = true
		// Also add alternate formats
		if len(id) > 0 {
			if id[0] == '$' {
				sessionSet["%"+id[1:]] = true // $55 -> %55
			} else if id[0] == '%' {
				sessionSet["$"+id[1:]] = true // %55 -> $55
			}
		}
	}

	return sessionSet, nil
}

// identityListingUnknown is the result when tmux cannot list its sessions and
// no lock could be judged without them: their state is unknown, not stale.
func identityListingUnknown(name string, err error) *CheckResult {
	return &CheckResult{
		Name:    name,
		Status:  StatusSkipped,
		Message: fmt.Sprintf("lock state unknown: could not list tmux sessions: %v", err),
		FixHint: "Check that tmux is running, then re-run 'gt doctor'",
	}
}

func (c *IdentityCollisionCheck) Fix(ctx *CheckContext) error {
	cleaned, err := lock.CleanStaleLocks(ctx.TownRoot)
	if err != nil {
		return fmt.Errorf("cleaning stale locks: %w", err)
	}

	if cleaned > 0 {
		fmt.Printf("  Cleaned %d stale lock(s)\n", cleaned)
	}

	return nil
}
