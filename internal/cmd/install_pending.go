package cmd

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/installpending"
)

// A gt install replaces the binary a fresh sling's session would run, so no
// sling starts while one is pending: `gt sling` refuses every hand sling, and
// the spec dispatcher refuses its tick and each sling a planned job would make
// (gt-2rp64). installpending.Store.Pending answers whether an install is in
// flight, so a stale marker or a dead writer never holds a sling.

// installPendingNote names the install a new sling may not start under, and
// what lifts it.
func installPendingNote(m *installpending.Marker) string {
	return fmt.Sprintf("an install of gt is pending (recorded %s by daemon pid %d); no new sling starts until it lands or its marker clears",
		m.At.UTC().Format(time.RFC3339), m.PID)
}

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
	return fmt.Errorf("%s %s", dispatch.SlingRefusalMarker, installPendingNote(m))
}

// refuseWhileInstallPending is gt sling's refusal for this run, or nil when no
// install is pending. A run with no reader wired refuses too: it cannot see an
// install, so it must not start a session beside one (gt-2rp64).
func (r *slingRun) refuseWhileInstallPending(townRoot string) error {
	if r.pendingInstall == nil {
		return installPendingRefusal(nil, fmt.Errorf("the pending-install reader is not wired"))
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
	m, err := e.PendingInstall(e.Now())
	if err != nil {
		return "", "install-pending marker unreadable, not dispatching: " + err.Error()
	}
	if m == nil {
		return "", ""
	}
	return installPendingNote(m), ""
}
