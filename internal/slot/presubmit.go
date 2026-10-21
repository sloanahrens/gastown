package slot

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/procid"
)

// The presubmit pool caps how many gt done local gates run at once townwide.
// It is a pool of its own, not the container-gate pool: a presubmit never takes
// a landing slot, and a landing never waits behind a presubmit.
//
// Like the container pool it is a set of kernel-released flocks, so a holder
// that dies releases its slot without any reclaim path. The owner file beside
// each lock is display-only metadata for gt slot status.

// PresubmitWaitLogAfter is how long a presubmit wait runs before it is logged
// once: a wait that long is worth a line in the caller's output, and a shorter
// one is just the gate starting.
const PresubmitWaitLogAfter = time.Minute

// PresubmitLockPath returns the flock file for presubmit slot i.
func PresubmitLockPath(townRoot string, i int) string {
	return filepath.Join(LockDir(townRoot), fmt.Sprintf("presubmit-%d.lock", i))
}

// PresubmitOwnerPath returns the display-only owner file for presubmit slot i.
func PresubmitOwnerPath(townRoot string, i int) string {
	return filepath.Join(LockDir(townRoot), fmt.Sprintf("presubmit-%d.owner", i))
}

// PresubmitSize is the number of presubmit slots townRoot's settings/config.json
// describes (operational.daemon.presubmit_pool).
func PresubmitSize(townRoot string) int {
	return config.LoadOperationalConfig(townRoot).GetDaemonConfig().PresubmitPoolV()
}

// AcquirePresubmit blocks until one of size presubmit slots is free, then holds
// it. timeout <= 0 waits indefinitely. role is the caller's identity, recorded
// in the owner file and the events (the rig is read from it, as for the
// container gate). A wait longer than PresubmitWaitLogAfter is logged once to
// the gate's probe output.
func AcquirePresubmit(townRoot, role string, timeout time.Duration, size int) (*Handle, error) {
	return NewGate().AcquirePresubmit(townRoot, role, timeout, size)
}

// AcquirePresubmit is the package-level AcquirePresubmit on this gate.
func (g *Gate) AcquirePresubmit(townRoot, role string, timeout time.Duration, size int) (*Handle, error) {
	if size < 1 {
		size = config.DefaultPresubmitPool
	}
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}

	start := g.clock.Now()
	deadline := start.Add(timeout)
	waitLogged := false
	logLongWait := func() {
		if waitLogged || g.clock.Since(start) <= PresubmitWaitLogAfter {
			return
		}
		waitLogged = true
		fmt.Fprintf(g.probeOut, "gt slot: %s has waited %s for a presubmit slot (%d in the pool)\n",
			role, g.clock.Since(start).Round(time.Second), size)
	}

	for {
		for i := 0; i < size; i++ {
			unlock, ok, err := lock.FlockTryAcquire(PresubmitLockPath(townRoot, i))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			at := g.clock.Now()
			writePresubmitOwner(townRoot, i, role, g.pid, at)
			waited := g.clock.Since(start)
			logLongWait()
			info := waitInfo{Waited: waited, Timeout: timeout}
			emitPresubmitWait(g.probeOut, townRoot, role, i, info)
			return &Handle{
				gate:       g,
				townRoot:   townRoot,
				unlock:     pinHold(unlock),
				Index:      i,
				WaitedFor:  waited,
				role:       role,
				acquiredAt: at,
				ownerPath:  PresubmitOwnerPath(townRoot, i),
				presubmit:  true,
			}, nil
		}
		logLongWait()
		if timeout > 0 && g.clock.Now().After(deadline) {
			info := waitInfo{Waited: g.clock.Since(start), Timeout: timeout, TimedOut: true}
			emitPresubmitWait(g.probeOut, townRoot, role, -1, info)
			return nil, fmt.Errorf("timed out after %s waiting for a presubmit slot (pool of %d)", timeout.Round(time.Second), size)
		}
		g.clock.Sleep(g.pollInterval)
	}
}

// writePresubmitOwner publishes presubmit slot i's holder metadata once its
// flock is taken. A failed write is not fatal: the flock is the truth, and the
// cost of a missing file is a status line without a role.
func writePresubmitOwner(townRoot string, i int, role string, pid int, at time.Time) {
	start, _ := procid.StartToken(pid)
	_ = atomicfile.EnsureDirAndWriteJSON(PresubmitOwnerPath(townRoot, i), Owner{
		Role:       role,
		PID:        pid,
		Start:      start,
		AcquiredAt: at,
		Slot:       i,
		Settled:    true,
	})
}

// StatusPresubmit reports the presubmit pool's size slots: which are held and by
// whom. It probes each flock without blocking, so it is safe to call while
// presubmits run.
func StatusPresubmit(townRoot string, size int) (Report, error) {
	if size < 1 {
		size = config.DefaultPresubmitPool
	}
	var rep Report
	for i := 0; i < size; i++ {
		st := SlotState{Index: i}
		path := PresubmitLockPath(townRoot, i)
		if _, statErr := os.Stat(path); statErr == nil || !os.IsNotExist(statErr) {
			unlock, ok, err := lock.FlockTryAcquire(path)
			if err != nil {
				return Report{}, err
			}
			if ok {
				// Remove the owner file under the flock, as StatusPoolLocksOnly
				// does, so a removal never deletes the next holder's file.
				_ = os.Remove(PresubmitOwnerPath(townRoot, i))
				unlock()
			} else {
				st.Held = true
				st.Owner = readOwnerFile(PresubmitOwnerPath(townRoot, i))
				if st.Owner != nil {
					st.Owner.Slot = i
				}
				rep.HeldCount++
				if !rep.Held {
					rep.Held = true
					rep.Owner = st.Owner
				}
			}
		}
		rep.Slots = append(rep.Slots, st)
	}
	rep.Total = len(rep.Slots)
	return rep, nil
}

// emitPresubmitWait emits the slot_wait event for a presubmit grant, or for a
// caller that gave up. slot is -1 when no slot was won. The payload carries
// pool "presubmit" so readers can tell it from a container-gate wait.
func emitPresubmitWait(w io.Writer, townRoot, role string, slot int, info waitInfo) {
	outcome := "granted"
	message := fmt.Sprintf("%s acquired presubmit slot %d after waiting %s", role, slot, info.Waited.Round(time.Second))
	if info.TimedOut {
		outcome = "timeout"
		message = fmt.Sprintf("%s gave up on a presubmit slot after waiting %s (cap %s)",
			role, info.Waited.Round(time.Second), info.Timeout.Round(time.Second))
	}
	payload := map[string]interface{}{
		"pool":      "presubmit",
		"role":      role,
		"slot":      slot,
		"waited_s":  info.Waited.Seconds(),
		"timeout_s": info.Timeout.Seconds(),
		"outcome":   outcome,
		"message":   message,
	}
	if rig := rigFromRole(role); rig != "" {
		payload["rig"] = rig
	}
	if err := events.LogTo(townRoot, events.TypeSlotWait, events.ActorGt, payload, events.VisibilityBoth); err != nil {
		fmt.Fprintf(w, "gt slot: recording presubmit slot_wait event: %v\n", err)
	}
}

// emitPresubmitHold emits the slot_hold event for a presubmit release.
func emitPresubmitHold(w io.Writer, townRoot, role string, slot int, held time.Duration) {
	payload := map[string]interface{}{
		"pool":    "presubmit",
		"role":    role,
		"slot":    slot,
		"held_s":  held.Seconds(),
		"message": fmt.Sprintf("%s released presubmit slot %d after %s", role, slot, held.Round(time.Second)),
	}
	if rig := rigFromRole(role); rig != "" {
		payload["rig"] = rig
	}
	if err := events.LogTo(townRoot, events.TypeSlotHold, events.ActorGt, payload, events.VisibilityBoth); err != nil {
		fmt.Fprintf(w, "gt slot: recording presubmit slot_hold event: %v\n", err)
	}
}
