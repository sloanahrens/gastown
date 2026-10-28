package cmd

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/installpending"
)

// A gt install replaces the binary a fresh sling's session would run, so no
// sling starts while one is pending: `gt sling` refuses every hand sling, the
// spec dispatcher refuses its tick and each sling a planned job would make, and
// the queue dispatch cycle holds before it plans (gt-2rp64, gt-c0nvv).
// installpending.Store.Pending answers whether an install is in flight, so a
// stale marker or a dead writer never holds a sling. The sentence naming the
// install is installpending.Marker.Note, so every dispatcher says it the same
// way.

// installPendingRefusal is the error a gt sling returns while m stands. It
// leads with dispatch.SlingRefusalMarker, so an automatic dispatcher defers the
// bead instead of counting a failure. err is a marker read that failed, which
// refuses too: a caller that must not race an install never reads a broken
// marker as none. nil m and nil err is no install pending.
func installPendingRefusal(m *installpending.Marker, err error) error {
	if err != nil {
		return fmt.Errorf("%s the pending-install marker cannot be read, so an install may be in flight: %w", dispatch.SlingRefusalMarker, err)
	}
	if m == nil {
		return nil
	}
	return fmt.Errorf("%s %s", dispatch.SlingRefusalMarker, m.Note())
}

// refuseWhileInstallPending is gt sling's refusal for this run, or nil when no
// install is pending.
func (r *slingRun) refuseWhileInstallPending(townRoot string) error {
	if r.pendingInstall == nil {
		return nil
	}
	return installPendingRefusal(r.pendingInstall(townRoot, time.Now()))
}

// installPendingStop is what a pending install does to a sling the spec
// dispatcher is about to make: the hold sentence that stops it, or the error
// line for a marker that cannot be read (which stops it too). Both empty is no
// install pending, and a tick with no reader reads no marker.
func (e specDispatchEnv) installPendingStop() (hold, errLine string) {
	if e.PendingInstall == nil {
		return "", ""
	}
	m, err := e.PendingInstall()
	if err != nil {
		return "", "install-pending marker unreadable, not dispatching: " + err.Error()
	}
	if m == nil {
		return "", ""
	}
	return m.Note(), ""
}
