package slot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// GateIntentTTL is how long a registered gate intent keeps non-gate starts
// yielding without being refreshed. A refinery refreshes it every time it picks
// the next MR, and clears it when the queue is empty; the TTL is only for an
// intent nobody comes back for (the refinery died holding it). It covers the
// refinery's wait for host load to drop before a gate plus the gate's start,
// and the per-acquire MaxGateYield bounds each waiter regardless.
const GateIntentTTL = 30 * time.Minute

// GateIntent records that a rig's refinery has a ready MR and is about to run
// a gate, before it holds any slot (gt-22hdp.29). The refinery checks host
// load before it acquires a slot, so under crew load a gate that only counted
// once it held a slot would never start and nothing would ever yield to it.
// An intent closes that loop: new non-gate starts wait on it as on a held
// gate slot, running suites drain, and load falls to where the gate starts.
type GateIntent struct {
	Role         string    `json:"role"`
	Ref          string    `json:"ref,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// gateIntentPrefix names intent files in LockDir. They never match
// slotIndexFromLockPath's "container-gate-<n>.lock", so they are never slots.
const gateIntentPrefix = "gate-intent-"

// gateIntentPath is rig's intent file.
func gateIntentPath(townRoot, rig string) string {
	return filepath.Join(LockDir(townRoot), gateIntentPrefix+filepath.Base(rig)+".json")
}

// RegisterGateIntent records (or refreshes) rig's pending gate for ref, the MR
// its refinery is about to gate.
func RegisterGateIntent(townRoot, rig, ref string) error {
	return NewGate().RegisterGateIntent(townRoot, rig, ref)
}

// RegisterGateIntent is the package-level RegisterGateIntent on this gate.
func (g *Gate) RegisterGateIntent(townRoot, rig, ref string) error {
	if rig == "" {
		return errors.New("registering gate intent: empty rig")
	}
	now := g.clock.Now()
	return atomicfile.EnsureDirAndWriteJSON(gateIntentPath(townRoot, rig), GateIntent{
		Role:         rig + "/refinery",
		Ref:          ref,
		RegisteredAt: now,
		ExpiresAt:    now.Add(GateIntentTTL),
	})
}

// ClearGateIntent removes rig's pending gate. Clearing nothing is not an error.
func ClearGateIntent(townRoot, rig string) error {
	return NewGate().ClearGateIntent(townRoot, rig)
}

// ClearGateIntent is the package-level ClearGateIntent on this gate.
func (g *Gate) ClearGateIntent(townRoot, rig string) error {
	if err := os.Remove(gateIntentPath(townRoot, rig)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing gate intent: %w", err)
	}
	return nil
}

// pendingGate returns the oldest unexpired gate intent in townRoot, if any.
// Unreadable intent files are ignored: a corrupt file must not stall the town.
func (g *Gate) pendingGate(townRoot string) (*GateIntent, bool) {
	entries, err := os.ReadDir(LockDir(townRoot))
	if err != nil {
		return nil, false
	}
	now := g.clock.Now()
	var live []*GateIntent
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, gateIntentPrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(LockDir(townRoot), name)) //nolint:gosec // path derives from the town root
		if err != nil {
			continue
		}
		var in GateIntent
		if json.Unmarshal(data, &in) != nil || in.expired(now) {
			continue
		}
		live = append(live, &in)
	}
	if len(live) == 0 {
		return nil, false
	}
	sort.Slice(live, func(i, j int) bool { return live[i].RegisteredAt.Before(live[j].RegisteredAt) })
	return live[0], true
}

// expired reports whether the intent no longer holds anyone up at now. The
// file's own ExpiresAt is trusted only up to RegisteredAt + GateIntentTTL, and
// an intent stamped in the future (clock skew, a hand-edited file) is treated
// as expired rather than as one that could last indefinitely.
func (in GateIntent) expired(now time.Time) bool {
	if in.RegisteredAt.After(now) {
		return true
	}
	end := in.RegisteredAt.Add(GateIntentTTL)
	if in.ExpiresAt.Before(end) {
		end = in.ExpiresAt
	}
	return !now.Before(end)
}
