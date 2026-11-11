package daemon

import (
	"github.com/steveyegge/gastown/internal/dispatch"
)

// logDispatchHoldView logs the summary of the dispatch hold view when it
// changes since the last heartbeat, so an operator reading the daemon log sees
// what the daemon reads from the hold sources, and when that changes. The view
// is read again on every call; nothing is cached.
func (d *Daemon) logDispatchHoldView() {
	summary := dispatch.HoldView(d.config.TownRoot).Summary()
	if d.dispatchHoldView.Changed(summary) {
		d.logger.Printf("Dispatch hold view: %s", summary)
	}
}
