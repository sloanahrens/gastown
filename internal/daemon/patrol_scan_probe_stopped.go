package daemon

import (
	"github.com/steveyegge/gastown/internal/session"
)

// probeErrorStopped reports whether a sandbox whose session probe errored is
// Stopped at its gateway, so the record walk can let it go instead of keeping it
// for ever (gt-zqxlmu). A gateway that cannot answer is not proof of Stopped: the
// read's error reads as false, and the call still spends its one ask.
func (h *patrolScanHost) probeErrorStopped(rec session.SandboxRecord) bool {
	h.asks.spend()
	phase, err := h.sandboxPhase(rec.Gateway, rec.Session)
	if err != nil {
		return false
	}
	return isStoppedPhase(phase)
}
