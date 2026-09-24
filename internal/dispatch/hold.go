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
// root whose existence parks automatic dispatch. It is the file the
// seat-refill plugin already honors (plugins/seat-refill/run.sh), so one
// `touch` stops both the nudges that ask for a sling and the code paths that
// sling on their own.
const HoldFileName = "seat-refill.hold"

// HoldFileEnv relocates the hold file, with the same ${VAR:-default}
// semantics as run.sh: set and non-empty, it names the hold file; unset or
// empty, the hold is <town>/seat-refill.hold.
const HoldFileEnv = "GT_SEAT_REFILL_HOLD"

// HoldFilePath returns the operator hold file for a town.
func HoldFilePath(townRoot string) string {
	if p := os.Getenv(HoldFileEnv); p != "" {
		return p
	}
	return filepath.Join(townRoot, HoldFileName)
}

// OperatorHold reports why automatic dispatch must not run in this town right
// now, or "" when it may. It answers for the operator's hold file and for a
// town-wide ESTOP, the same two hand brakes seat-refill checks.
//
// Only automatic dispatchers consult it. An explicit `gt sling` typed by an
// operator or the mayor is the decision the hold defers to, and stays open.
//
// A hold path that cannot be stat'ed for any reason other than not existing
// fails closed: the hold cannot be ruled out, and dispatching through a hold
// is the outcome it exists to prevent (gt-ifijm).
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
	if estop.IsActive(townRoot) {
		return fmt.Sprintf("town ESTOP active (%s)", estop.FilePath(townRoot))
	}
	return ""
}

// RigHold is OperatorHold plus the per-rig ESTOP (<town>/ESTOP.<rig>), for a
// dispatcher that knows which rig it is about to sling into. seat-refill
// honors the same per-rig file (run.sh). An empty rig answers for the town.
func RigHold(townRoot, rig string) string {
	if reason := OperatorHold(townRoot); reason != "" {
		return reason
	}
	if townRoot != "" && rig != "" && estop.IsRigActive(townRoot, rig) {
		return fmt.Sprintf("rig ESTOP active (%s)", estop.RigFilePath(townRoot, rig))
	}
	return ""
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
