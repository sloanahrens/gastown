// Package dispatch holds the contracts every automatic dispatcher in the town
// shares — the spec dispatcher and the daemon's scheduled slings. The callers
// cannot import each other freely, so the shared rules live here.
package dispatch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// HoldFileName is the operator's town-wide dispatch hold: a file at the town
// root whose existence parks automatic dispatch. The name is the deleted
// seat-refill plugin's (gt-4k3fj.8.8, plugins/seat-refill/run.sh); it is kept
// so a hold file an operator wrote then still parks the dispatcher that
// replaced the plugin (the spec dispatcher and gt sling), and every other
// automatic sling path with it.
const HoldFileName = "seat-refill.hold"

// HoldFilePath returns the operator hold file for a town.
func HoldFilePath(townRoot string) string {
	return filepath.Join(townRoot, HoldFileName)
}

// BootHealthPath returns the file the daemon publishes its boot reason in: the
// daemon's own directory, read by dispatchers in other processes.
func BootHealthPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "boot-health")
}

// BootHold returns the boot reason the daemon published at BootHealthPath, or
// "" when the file does not exist. A file that exists but cannot be read, or
// that names no reason, holds, because the daemon's answer cannot be ruled out.
// An empty town root holds too, since the file cannot be located to rule it
// out. A file left by a dead daemon holds on purpose (fail closed); to clear
// it, remove daemon/boot-health under the town root by hand.
func BootHold(townRoot string) string {
	if townRoot == "" {
		return "no town root to read the boot health from; treating as held"
	}
	path := BootHealthPath(townRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("boot health unreadable (%v); treating as held", err)
	}
	reason, _, _ := strings.Cut(string(data), "\n")
	if reason == "" {
		reason = "boot health file names no reason"
	}
	return fmt.Sprintf("%s (%s); remove the file by hand if the daemon is retired", reason, path)
}

// OperatorHold reports why automatic dispatch must not run in this town right
// now, or "" when it may. It answers for the operator's hold file, for a
// town-wide ESTOP, for the daemon's boot hold file and for the Ring when it is
// Waned, the holds the seat-refill plugin checked and the spec dispatcher
// checks now. It is the one dispatch choke point for e-stop (gt-4k3fj.4);
// restarts and kills have theirs in internal/supervisor.
//
// Only automatic dispatchers consult it. An explicit `gt sling` typed by an
// operator is the decision the hold file defers to; an e-stop, the boot hold
// and a Waned Ring refuse it too, in the sling path itself.
//
// A hold path or ESTOP sentinel that cannot be stat'ed for any reason other
// than not existing fails closed: the hold cannot be ruled out, and
// dispatching through a hold is the outcome it exists to prevent (gt-ifijm,
// gt-e7lqk). A town root that does not exist holds too (HoldView).
func OperatorHold(townRoot string) string {
	return operatorHold(townRoot, ring.TownKey(os.Getenv), time.Now())
}

// operatorHold is OperatorHold over the Ring's key resolver and clock, so a
// test judges the Ring without reading the process environment.
func operatorHold(townRoot string, key ring.KeyFunc, now time.Time) string {
	return holdView(townRoot, key, now, noRigs).Held()
}

// HoldSource is one hold the dispatcher honors, and what it reads for it.
type HoldSource struct {
	// Name is the source's name in the view, as the tests and the reasons
	// refer to it.
	Name string
	// Reason says why the source holds, or is "" when it does not.
	Reason string
}

// HoldState is what the dispatcher reads from a town's hold sources on one
// call: the state of each source, and the summary the operator reads.
type HoldState struct {
	// Sources are the town-wide holds, in the order OperatorHold checks them.
	Sources []HoldSource
	// Rigs are the rig ESTOPs the town directory lists that are active. A rig
	// ESTOP holds its own rig (RigHold), not the town, so Held does not read
	// it; Summary does.
	Rigs []HoldSource
	// RigsNote says why the rig ESTOPs are unknown rather than listed: the
	// town root is missing, or the town directory cannot be read. "" when
	// Rigs is the complete listing.
	RigsNote string
}

// Held returns the reason the first town-wide hold gives, or "" when none
// holds. It is the answer OperatorHold gives.
func (s HoldState) Held() string {
	for _, source := range s.Sources {
		if source.Reason != "" {
			return source.Reason
		}
	}
	return ""
}

// Summary is one line for the operator. It is the held reason when a
// town-wide hold holds. It says clear only when Held is "" and no rig ESTOP
// is active and the rig listing is complete, so "clear" always means
// OperatorHold would return "". An active rig ESTOP reads as not clear, with
// the note that the town itself is open.
func (s HoldState) Summary() string {
	if reason := s.Held(); reason != "" {
		return reason
	}
	if s.RigsNote != "" {
		return "not clear: " + s.RigsNote
	}
	if len(s.Rigs) > 0 {
		reasons := make([]string, 0, len(s.Rigs))
		for _, rig := range s.Rigs {
			reasons = append(reasons, rig.Reason)
		}
		return "not clear: " + strings.Join(reasons, "; ") + "; the town itself is not held"
	}
	return "clear"
}

// HoldView reports the hold sources the dispatcher reads for townRoot right
// now: each town-wide hold, and each rig ESTOP the town directory lists. Every
// call reads the paths again; nothing is cached. The Summary it gives is clear
// only when OperatorHold would return "".
func HoldView(townRoot string) HoldState {
	return holdView(townRoot, ring.TownKey(os.Getenv), time.Now(), listRigESTOPs)
}

// rigLister returns the rig names that have an ESTOP.<rig> file in the town
// directory. A view calls it once.
type rigLister func(townRoot string) ([]string, error)

// listRigESTOPs reads the town directory once and returns the rig names of its
// ESTOP.<rig> files.
func listRigESTOPs(townRoot string) ([]string, error) {
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil, err
	}
	prefix := estop.RigFileName("")
	var rigs []string
	for _, entry := range entries {
		if rig, ok := strings.CutPrefix(entry.Name(), prefix); ok && rig != "" {
			rigs = append(rigs, rig)
		}
	}
	return rigs, nil
}

// noRigs lists no rigs. OperatorHold uses it: a rig ESTOP holds its own rig
// and not the town, so the town-wide answer does not read the directory.
func noRigs(string) ([]string, error) { return nil, nil }

// holdView is the one reading of a town's hold sources, shared by HoldView and
// OperatorHold. It takes the Ring's key resolver, the clock and the rig lister
// so a test judges each source without reading the process environment or a
// real town directory.
func holdView(townRoot string, key ring.KeyFunc, now time.Time, rigs rigLister) HoldState {
	var state HoldState
	if townRoot == "" {
		return state
	}
	if _, err := os.Stat(townRoot); errors.Is(err, fs.ErrNotExist) {
		// Its sources cannot be read under a missing town root, so no hold
		// under it can be ruled out, and its rig ESTOPs are unknown, not clear.
		state.Sources = []HoldSource{{
			Name:   "town root",
			Reason: fmt.Sprintf("town root missing (%s); treating as held", townRoot),
		}}
		state.RigsNote = "town root missing; rig ESTOPs unknown"
		return state
	}
	state.Sources = []HoldSource{
		{Name: "operator hold file", Reason: operatorHoldFileReason(townRoot)},
		{Name: "town ESTOP", Reason: estopHold(townRoot, "")},
		{Name: "boot hold", Reason: BootHold(townRoot)},
		{Name: "Ring", Reason: ring.Hold(townRoot, key, now)},
	}
	names, err := rigs(townRoot)
	if err != nil {
		state.RigsNote = fmt.Sprintf("rig ESTOPs unlisted (%v)", err)
		return state
	}
	for _, rig := range names {
		if reason := rigEstopReason(townRoot, rig); reason != "" {
			state.Rigs = append(state.Rigs, HoldSource{Name: "rig ESTOP " + rig, Reason: reason})
		}
	}
	return state
}

// operatorHoldFileReason is the operator hold file's reason, or "" when the
// file does not exist. A path that cannot be stat'ed for another reason holds.
func operatorHoldFileReason(townRoot string) string {
	path := HoldFilePath(townRoot)
	if _, err := os.Stat(path); err == nil {
		return fmt.Sprintf("operator dispatch hold present (%s); remove it to resume", path)
	} else if !os.IsNotExist(err) {
		return fmt.Sprintf("operator dispatch hold unreadable (%v); treating as held", err)
	}
	return ""
}

// rigEstopReason is the reason rig's own ESTOP file holds that rig, or "" when
// the file does not exist. A file that cannot be stat'ed for another reason
// holds, as estopHold does.
func rigEstopReason(townRoot, rig string) string {
	path := estop.RigFilePath(townRoot, rig)
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Sprintf("rig ESTOP active (%s)", path)
	case os.IsNotExist(err):
		return ""
	default:
		return fmt.Sprintf("rig ESTOP unreadable (%v); treating as held", err)
	}
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

// RigHold is OperatorHold plus the holds that cover one rig, for a dispatcher
// that knows which rig it is about to sling into: the rig's ESTOP
// (<town>/ESTOP.<rig>) and the operator's dispatch hold on its registry entry
// (townconfig.DispatchHeldReason). An empty rig answers for the town, and the
// town-wide holds win because they are checked first.
func RigHold(townRoot, rig string) string {
	if reason := OperatorHold(townRoot); reason != "" {
		return reason
	}
	if townRoot == "" || rig == "" {
		return ""
	}
	if reason := estopHold(townRoot, rig); reason != "" {
		return reason
	}
	return townconfig.DispatchHeldReason(townRoot, rig)
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
