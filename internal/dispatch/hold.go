// Package dispatch holds the contracts every automatic dispatcher in the town
// shares — the daemon's convoy feeders and scheduled_slings patrol, and the
// deacon's RECOVERED_BEAD redispatch. It is a leaf package so that daemon,
// deacon and convoy, which cannot import each other freely, can all reach it.
package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/steveyegge/gastown/internal/estop"
)

// HoldFileName is the operator's town-wide dispatch hold: a file at the town
// root whose existence parks automatic dispatch. Every reader of it — spec
// dispatch, the convoy feeders, the scheduler, scheduled_slings and the
// daemon's dispatch step — reads this constant, so one `touch` stops both the
// nudges that ask for a sling and the code paths that sling on their own. The
// name predates the Go readers, from the deleted seat-refill plugin
// (gt-4k3fj.8.6); an operator's hand brake is on disk under it.
const HoldFileName = "seat-refill.hold"

// HoldFilePath returns the operator hold file for a town.
func HoldFilePath(townRoot string) string {
	return filepath.Join(townRoot, HoldFileName)
}

// OperatorHold reports why automatic dispatch must not run in this town right
// now, or "" when it may. It answers for the operator's hold file and for a
// town-wide ESTOP, the same two hand brakes seat-refill checks. It is the one
// dispatch choke point for e-stop (gt-4k3fj.4); restarts and kills have
// theirs in internal/supervisor.
//
// Only automatic dispatchers consult it. An explicit `gt sling` typed by an
// operator or the mayor is the decision the hold file defers to; an e-stop
// refuses it too, in the sling path itself.
//
// A hold path or ESTOP sentinel that cannot be stat'ed for any reason other
// than not existing fails closed: the hold cannot be ruled out, and
// dispatching through a hold is the outcome it exists to prevent (gt-ifijm,
// gt-e7lqk).
func OperatorHold(townRoot string) string {
	if townRoot == "" {
		return ""
	}
	path := HoldFilePath(townRoot)
	if _, err := os.Stat(path); err == nil {
		return fmt.Sprintf("operator dispatch hold present (%s); remove it to resume", path)
	} else if !os.IsNotExist(err) {
		return fmt.Sprintf("operator dispatch hold unreadable (%v); treating as held", err)
	}
	return estopHold(townRoot, "")
}

// estopHold reports the e-stop that covers rig ("" for the town sentinel
// alone), failing closed like the supervisor's check.
func estopHold(townRoot, rig string) string {
	on, err := estop.ActiveFor(townRoot, rig)
	switch {
	case err != nil:
		return fmt.Sprintf("ESTOP unreadable (%v); treating as held", err)
	case !on:
		return ""
	case estop.IsActive(townRoot):
		return fmt.Sprintf("town ESTOP active (%s)", estop.FilePath(townRoot))
	default:
		return fmt.Sprintf("rig ESTOP active (%s)", estop.RigFilePath(townRoot, rig))
	}
}

// RigHold is OperatorHold plus the per-rig ESTOP (<town>/ESTOP.<rig>), for a
// dispatcher that knows which rig it is about to sling into. seat-refill
// honors the same per-rig file (run.sh). An empty rig answers for the town.
func RigHold(townRoot, rig string) string {
	if reason := OperatorHold(townRoot); reason != "" {
		return reason
	}
	if townRoot == "" || rig == "" {
		return ""
	}
	return estopHold(townRoot, rig)
}

// HoldLatch remembers the last hold reason a polling dispatcher saw, so it can
// log a hold once when it appears, changes, or lifts rather than on every
// tick. The zero value is ready to use and starts "not held".
type HoldLatch struct {
	mu   sync.Mutex
	last string
}

// Changed records reason as the current hold state and reports whether it
// differs from the previous one.
func (l *HoldLatch) Changed(reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason == l.last {
		return false
	}
	l.last = reason
	return true
}
