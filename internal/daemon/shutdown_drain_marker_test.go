package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townflight"
)

// A command listed while the drain runs is refused with ErrDraining: the drain
// writes the town's marker before it takes its holds (gt-m9exgy).
func TestShutdownDrainMarkerRefusesRegisterDuringDrain(t *testing.T) {
	t.Parallel()
	town := drainTown(t)
	d, _ := signalTestDaemon(t)
	d.config.TownRoot = town
	d.drainBound = time.Hour
	var refused error
	d.onDrainHold = func() {
		_, refused = townflight.Register(town, "gt sling")
	}
	d.drainForShutdown(make(chan os.Signal))
	if !errors.Is(refused, townflight.ErrDraining) {
		t.Fatalf("Register during the drain = %v, want ErrDraining", refused)
	}
}

// The marker stays after the drain returns: the drain does not clear it, so a
// listed command is still refused until the next daemon's start clears it
// (gt-m9exgy).
func TestShutdownDrainMarkerStaysAfterTheDrain(t *testing.T) {
	t.Parallel()
	town := drainTown(t)
	d, _ := signalTestDaemon(t)
	d.config.TownRoot = town
	d.drainBound = time.Hour
	d.drainForShutdown(make(chan os.Signal))
	if _, err := townflight.Register(town, "gt sling"); !errors.Is(err, townflight.ErrDraining) {
		t.Fatalf("Register after the drain = %v, want ErrDraining", err)
	}
}

// A marker that cannot be written is logged with its error, and the drain still
// waits for the landing pass in flight before it returns (gt-m9exgy).
func TestShutdownDrainMarkerLogsAStartErrorAndStillWaitsForLanding(t *testing.T) {
	t.Parallel()
	town := drainTown(t)
	blockedMarker(t, town)
	d, logs := signalTestDaemon(t)
	d.config.TownRoot = town
	d.drainBound = time.Hour
	startErr := townflight.StartDrain(town)
	if startErr == nil {
		t.Fatalf("%s: StartDrain over a blocked marker = nil, want an error", t.Name())
	}
	if !d.beginLandingPass() {
		t.Fatalf("%s: no landing pass could start before the signal", t.Name())
	}
	holdTaken := make(chan struct{})
	d.onDrainHold = func() { close(holdTaken) }
	drained := make(chan os.Signal, 1)
	go func() { drained <- d.drainForShutdown(make(chan os.Signal)) }()

	reached(t, "the drain taking its hold", holdTaken)
	select {
	case sig := <-drained:
		t.Fatalf("%s: the drain returned %v while the landing pass was in flight", t.Name(), sig)
	default:
	}
	if !strings.Contains(logs.String(), startErr.Error()) {
		t.Errorf("%s: the drain did not log the StartDrain error %q; log: %q", t.Name(), startErr, logs.String())
	}

	d.landingPassEnded()
	if sig := within(t, signalWait, "the drain after the landing pass ends", drained); sig != nil {
		t.Errorf("%s: the drain returned the signal %v, want nil once the pass ended", t.Name(), sig)
	}
}
