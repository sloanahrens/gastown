package daemon

import (
	"fmt"
	"strings"
	"time"
)

// crashLoopEscalationInterval is how often the daemon re-escalates while a
// core agent remains in crash-loop skip mode (gt-e7h).
const crashLoopEscalationInterval = time.Hour

// isCoreAgent reports whether the agent is one whose outage must never be
// silent. Deacon, witnesses, and refineries are load-bearing: when the daemon
// stops restarting one of them, the town degrades until a human intervenes.
// Polecats and dogs are expendable workers; silent crash-loop skip is fine.
func isCoreAgent(agentID string) bool {
	return agentID == "deacon" ||
		strings.HasPrefix(agentID, "witness") ||
		strings.HasPrefix(agentID, "refinery")
}

// escalateCrashLoopSkip emits a HIGH escalation to the mayor when the daemon
// skips restarting a core agent due to crash-loop backoff. Without this, the
// agent stays down indefinitely with nothing but a log line every heartbeat
// (gt-e7h: Deacon down 1h25m, detected only by luck). Escalates on first
// entry into skip mode and re-escalates hourly while the skip persists;
// non-core agents are skipped silently.
func (d *Daemon) escalateCrashLoopSkip(agentID, detail string) {
	if !isCoreAgent(agentID) {
		return
	}
	if d.restartTracker == nil || !d.restartTracker.ShouldEscalateCrashLoop(agentID, crashLoopEscalationInterval) {
		return
	}
	if err := d.restartTracker.Save(); err != nil {
		d.logger.Printf("Warning: failed to save restart state after crash-loop escalation: %v", err)
	}

	msg := fmt.Sprintf("%s is in crash-loop backoff — daemon is SKIPPING restarts and the agent is DOWN until manually cleared. Run 'gt daemon clear-backoff %s' to reset.", agentID, agentID)
	if detail != "" {
		msg += " Detail: " + detail
	}
	d.escalate("crash-loop", msg)
}
