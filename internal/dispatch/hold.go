// Package dispatch holds the contracts every automatic dispatcher in the town
// shares — the daemon's convoy feeders and scheduled_slings patrol, and the
// deacon's RECOVERED_BEAD redispatch. It is a leaf package so that daemon,
// deacon and convoy, which cannot import each other freely, can all reach it.
package dispatch

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/estop"
)

// HoldFileName is the operator's town-wide dispatch hold: a file at the town
// root whose existence parks automatic dispatch. It is the file the
// seat-refill plugin already honors (plugins/seat-refill/run.sh), so one
// `touch` stops both the nudges that ask for a sling and the code paths that
// sling on their own.
const HoldFileName = "seat-refill.hold"

// HoldFilePath returns the operator hold file for a town.
func HoldFilePath(townRoot string) string {
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
