package daemon

import "sync"

// Names of the holds on new landing passes. Each job releases only its own, so
// a released upgrade restart does not free a pass while rebuild_om is still
// swapping the om those passes run against (gt-0hsf6).
const (
	landingHoldUpgradeRestart = "upgrade-restart"
	landingHoldRebuildOM      = "rebuild-om"
)

// landingHolds is the daemon's set of holds on new landing passes: the landing
// workers start no pass while any hold is taken, so a job that needs the town
// quiet stops new work and waits out what is in flight instead of competing
// with an endless queue (gt-0hsf6).
type landingHolds struct {
	mu    sync.Mutex
	holds map[string]*landingHold
}

// landingHold is one named hold: idle is closed once no landing pass is in
// flight, so a holder wakes on the pass end instead of polling for it.
type landingHold struct {
	idle chan struct{}
	once sync.Once
}

// hold takes the named hold and returns the channel closed when the landing
// passes in flight have ended. A hold of that name already taken keeps its
// channel, so every caller waits on the same pass end.
//
// The caller reads the in-flight count only after this returns: a pass that
// ends in between wakes the channel, and one that starts in between is refused
// by beginLandingPass, so neither is missed.
func (h *landingHolds) hold(name string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if held, ok := h.holds[name]; ok {
		return held.idle
	}
	if h.holds == nil {
		h.holds = make(map[string]*landingHold)
	}
	held := &landingHold{idle: make(chan struct{})}
	h.holds[name] = held
	return held.idle
}

// release drops the named hold: the landing workers start passes again.
func (h *landingHolds) release(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.holds, name)
}

// held reports whether any job holds new landing passes off.
func (h *landingHolds) held() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.holds) > 0
}

// heldBy reports whether the named job holds new landing passes off.
func (h *landingHolds) heldBy(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.holds[name]
	return ok
}

// passesIdle wakes every hold: no landing pass is in flight. It is called when
// the in-flight count reaches zero, so a woken holder reads a town that stays
// quiet while its hold is on.
func (h *landingHolds) passesIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, held := range h.holds {
		held.once.Do(func() { close(held.idle) })
	}
}

// beginLandingPass takes a slot in the in-flight count for a pass, unless a
// hold went on since the caller's last check. A pass that slipped in after
// that check would otherwise start beside the install or the restart the hold
// waits for; a refused pass gives its slot back at once (gt-0hsf6).
func (d *Daemon) beginLandingPass() bool {
	d.landingPasses.Add(1)
	if d.landingPassWindowFn != nil {
		d.landingPassWindowFn()
	}
	if !d.landingHolds.held() {
		return true
	}
	d.landingPassEnded()
	return false
}

// landingPassEnded leaves the in-flight count and, when the count reaches
// zero, wakes the holders waiting for a quiet town.
func (d *Daemon) landingPassEnded() {
	if n := d.landingPasses.Add(-1); n == 0 {
		d.landingHolds.passesIdle()
	}
}

// upgradeRestartDrains reports whether an upgrade restart holds the landing
// passes off. It answers for the restart's own work only: a rebuild_om hold
// parks no ticker and starts no sling wait (gt-0hsf6).
func (d *Daemon) upgradeRestartDrains() bool {
	return d.landingHolds.heldBy(landingHoldUpgradeRestart)
}

// landingPassDraining reports whether the landing pass in flight must stop
// claiming beads: any job's hold counts, the upgrade restart's and
// rebuild_om's alike. A pass that kept claiming under rebuild_om's hold would
// run the whole queue and the drain would wait out its bound for nothing
// (gt-0hsf6).
func (d *Daemon) landingPassDraining() bool {
	return d.landingHolds.held()
}
