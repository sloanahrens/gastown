package daemon

import (
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/manifestdata"
)

// ShutdownDrainBound is how long a SIGTERM waits for the landing pass and the
// slings in flight before the daemon shuts down without them. The value is
// manifestdata's: the pod's terminationGracePeriodSeconds is sized from the
// drain's sum of the bound, the heartbeat grace and the Dolt stop budget
// (manifestdata/shutdown.go, gt-99ys4n).
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
// draining flag stops spec-dispatch ticks and tier sweeps, the shutdown-drain
// hold stops the landing workers from starting a pass and makes a pass in flight
// stop claiming beads, and the spec-sling set stops every sling from starting.
// The landing step and the slings already in flight are waited for, together,
// up to ShutdownDrainBound: a sling's kill would come from the cancel the caller
// makes after this returns, so waiting here lets it finish (gt-d4jrdc). It
// returns the second shutdown signal when one arrives during the wait, or nil
// when both ended or the bound elapsed. The caller cancels the daemon context
// only after this returns, so the cancel never cuts a landing step or a sling
// short.
func (d *Daemon) drainForShutdown(sigChan <-chan os.Signal) os.Signal {
	d.draining.Store(true)
	// The holds are taken before the in-flight counts are read: a pass or a sling
	// that starts after its hold is refused, and one that ends in between wakes
	// its channel, so neither is missed (landing_hold.go, spec_slings.go stop).
	idle := d.landingHolds.hold(landingHoldShutdownDrain)
	queued, slingsIdle := d.specSlings.stop()
	d.reportNotStartedSlings(queued)
	landing := d.landingPasses.Load() > 0
	slings := d.specSlings.list()
	if d.onDrainHold != nil {
		d.onDrainHold()
	}
	if !landing && len(slings) == 0 {
		d.logger.Println("draining for shutdown: no landing pass in flight")
		return nil
	}
	// A channel nil in the select is never ready, so a wait that is not needed
	// drops out of the loop.
	var landingDone, slingsDone <-chan struct{}
	bound := d.shutdownDrainBound()
	if landing {
		landingDone = idle
		d.logger.Printf("draining for shutdown: waiting up to %s for %s", bound, d.landingStepInFlight())
	}
	if len(slings) > 0 {
		slingsDone = slingsIdle
		d.logger.Printf("draining for shutdown: waiting up to %s for %s", bound, specSlingsInFlight(slings))
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for landingDone != nil || slingsDone != nil {
		select {
		case <-landingDone:
			landingDone = nil
			d.logger.Println("draining for shutdown: landing pass finished")
		case <-slingsDone:
			slingsDone = nil
			d.logger.Println("draining for shutdown: slings in flight finished")
		case <-timer.C:
			if landingDone != nil {
				d.logger.Printf("draining for shutdown: landing pass still in flight after %s; shutting down without it", bound)
			}
			if slingsDone != nil {
				d.logger.Printf("draining for shutdown: sling still in flight after %s; shutting down without it", bound)
			}
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
			d.logger.Printf("draining for shutdown: second signal %v, shutting down without the work in flight", sig)
			return sig
		}
	}
	return nil
}

// specSlingsInFlight names the slings the drain waits on, by bead, for the
// drain's log line.
func specSlingsInFlight(slings []specSlingFlight) string {
	beads := make([]string, 0, len(slings))
	for _, f := range slings {
		beads = append(beads, "the sling of "+f.Bead)
	}
	return strings.Join(beads, ", ")
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
