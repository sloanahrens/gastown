package daemon

import (
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/manifestdata"
)

// ShutdownDrainBound is how long a SIGTERM waits for the landing pass in flight
// before the daemon shuts down without it. The value is manifestdata's: the pod's
// terminationGracePeriodSeconds is sized from the drain's sum of the bound, the
// heartbeat grace and the Dolt stop budget (manifestdata/shutdown.go, gt-99ys4n).
const ShutdownDrainBound = manifestdata.LandingStepDrainBound

// shutdownDrainBound is the bound a drain waits for: ShutdownDrainBound, unless a
// test set drainBound (a test seam on the Daemon, so the bound runs in
// milliseconds).
func (d *Daemon) shutdownDrainBound() time.Duration {
	if d.drainBound > 0 {
		return d.drainBound
	}
	return ShutdownDrainBound
}

// drainForShutdown is the SIGTERM drain. The daemon starts nothing new: the
// draining flag stops spec-dispatch ticks and tier sweeps, and the shutdown-drain
// hold stops the landing workers from starting a pass and makes a pass in flight
// stop claiming beads. The landing step in flight is waited for, up to
// ShutdownDrainBound. It returns the second shutdown signal when one arrives
// during the wait, or nil when the pass ended or the bound elapsed. The caller
// cancels the daemon context only after this returns, so the cancel never cuts a
// landing step short.
func (d *Daemon) drainForShutdown(sigChan <-chan os.Signal) os.Signal {
	d.draining.Store(true)
	// The hold is taken before the in-flight count is read: a pass that starts
	// after the hold is refused by beginLandingPass, and a pass that ends in
	// between wakes idle, so neither is missed (landing_hold.go).
	idle := d.landingHolds.hold(landingHoldShutdownDrain)
	if d.onDrainHold != nil {
		d.onDrainHold()
	}
	if d.landingPasses.Load() == 0 {
		d.logger.Println("draining for shutdown: no landing pass in flight")
		return nil
	}
	bound := d.shutdownDrainBound()
	d.logger.Printf("draining for shutdown: waiting up to %s for %s", bound, d.landingStepInFlight())
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		select {
		case <-idle:
			d.logger.Println("draining for shutdown: landing pass finished")
			return nil
		case <-timer.C:
			d.logger.Printf("draining for shutdown: landing pass still in flight after %s; shutting down without it", bound)
			return nil
		case sig := <-sigChan:
			if isLifecycleSignal(sig) {
				d.logger.Println("Received SIGUSR1 while draining: ignored")
				continue
			}
			if isReloadRestartSignal(sig) {
				d.logger.Println("Received reload-restart signal while draining: ignored")
				continue
			}
			d.logger.Printf("draining for shutdown: second signal %v, shutting down without the landing pass", sig)
			return sig
		}
	}
}

// landingStepInFlight names the landing steps the drain waits on, one per rig, for
// the drain's log line.
func (d *Daemon) landingStepInFlight() string {
	var steps []string
	d.landingStates.each(func(rig, bead string) {
		steps = append(steps, "the landing of "+bead+" on "+rig)
	})
	if len(steps) == 0 {
		return "a landing pass that has no bead in flight yet"
	}
	return strings.Join(steps, ", ")
}
