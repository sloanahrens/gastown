package daemon

// The image_pin_layers patrol (gt-rhrbyd): once a day it checks every
// digest-pinned image against its registry, so a layer the registry no longer
// serves is found before the next images build. This file is the patrol's
// skeleton: its config key and off-by-default ticker, its last-run record under
// .runtime, and the run seam a later bead fills with the check (gt-hpmk5n). The
// seam reads nothing yet and returns nil.
//
// The ticker is a check cadence, not a run cadence: an in-process ticker resets
// its countdown on every daemon start, so a ticker set to a day would never fire
// in a town that restarts oftener. Due-ness comes from the persisted record, so
// a restart reads the day the town already ran and skips it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

const (
	// imagePinLayersPatrol is the patrol's name in daemon.patrols and in
	// daemon.container_off.
	imagePinLayersPatrol = "image_pin_layers"

	// imagePinLayersDir is the patrol's directory under the town's .runtime,
	// holding the last-run record.
	imagePinLayersDir = "image-pin-layers"

	// imagePinLayersLastRunFile is the last-run record's name in that
	// directory: the clock reading of the run that last completed, so a tick in
	// the same day runs nothing.
	imagePinLayersLastRunFile = "last-run.json"

	// imagePinLayersCheckTick is how often the patrol asks whether today's run
	// has happened. It is a check, not the run cadence: the record decides
	// whether a tick runs, so a restart catches up within one check and a tick
	// in a day that already ran reads one small file.
	imagePinLayersCheckTick = time.Hour

	// imagePinLayersTickTimeout bounds a whole run, the registry reads
	// included, so a stuck request frees the patrol's single-flight guard.
	imagePinLayersTickTimeout = 10 * time.Minute

	// imagePinLayersJoinWait bounds how long Run waits for a run still going
	// after the ticker stopped. A run left past it finishes its own bounded
	// reads, and the next start needs nothing it wrote.
	imagePinLayersJoinWait = 10 * time.Second
)

// imagePinLayersDay is the day a run belongs to: the UTC calendar date, so a
// record written by one process and read by the next names the same day
// whenever either starts.
func imagePinLayersDay(at time.Time) string {
	return at.UTC().Format(time.DateOnly)
}

// imagePinLayersLastRunPath is the record's path in town: under the patrol's
// own directory in .runtime, beside nothing else the town writes.
func imagePinLayersLastRunPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), imagePinLayersDir, imagePinLayersLastRunFile)
}

// imagePinLayersLastRun is the record's on-disk shape: the clock reading of the
// run that last completed.
type imagePinLayersLastRun struct {
	LastRun time.Time `json:"last_run"`
}

// readImagePinLayersLastRun returns the last completed run's time and whether
// the record names one. A missing record is no run yet rather than an error; a
// record that will not open or parse is an error, and the caller decides what a
// town without a readable record does.
func readImagePinLayersLastRun(path string) (time.Time, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading the last-run record %s: %w", path, err)
	}
	var rec imagePinLayersLastRun
	if err := json.Unmarshal(data, &rec); err != nil {
		return time.Time{}, false, fmt.Errorf("parsing the last-run record %s: %w", path, err)
	}
	if rec.LastRun.IsZero() {
		return time.Time{}, false, nil
	}
	return rec.LastRun, true, nil
}

// writeImagePinLayersLastRun records the run that just completed, atomically,
// so a reader sees either the previous record or this one.
func writeImagePinLayersLastRun(path string, at time.Time) error {
	return atomicfile.EnsureDirAndWriteJSON(path, imagePinLayersLastRun{LastRun: at})
}

// imagePinLayersDueDecision is whether this tick is today's run, with the line
// the caller logs either way.
type imagePinLayersDueDecision struct {
	due  bool
	note string
}

// imagePinLayersDue decides whether now falls on a day the town has not run in.
// The record is the day's authority, so a daemon that restarts inside the day
// decides as the one that ran did. A record that cannot be read makes today due
// and says so: a check skipped on a broken record starves the layer check
// silently, while a run only reads the registries again.
func imagePinLayersDue(recordPath string, now time.Time) imagePinLayersDueDecision {
	last, found, err := readImagePinLayersLastRun(recordPath)
	if err != nil {
		return imagePinLayersDueDecision{
			due:  true,
			note: fmt.Sprintf("the last-run record cannot be read (%v), so today's run is due", err),
		}
	}
	if !found {
		return imagePinLayersDueDecision{due: true, note: "no last-run record, so today's run is due"}
	}
	today, lastDay := imagePinLayersDay(now), imagePinLayersDay(last)
	if today == lastDay {
		return imagePinLayersDueDecision{
			note: fmt.Sprintf("already ran on %s (last run %s), so nothing is due", today, last.UTC().Format(time.RFC3339)),
		}
	}
	return imagePinLayersDueDecision{
		due:  true,
		note: fmt.Sprintf("the last run was %s, so %s's run is due", lastDay, today),
	}
}

// imagePinLayersRunFunc is one day's check: the error that ended it, or nil. It
// must return when ctx is done, so the run's bound frees the patrol's
// single-flight guard.
type imagePinLayersRunFunc func(ctx context.Context, at time.Time) error

// imagePinLayersRunSkeleton is this build's run: the check itself is a later
// bead's (gt-hpmk5n), so it reads no registry and raises nothing.
func imagePinLayersRunSkeleton(context.Context, time.Time) error {
	return nil
}

// startImagePinLayersTicker starts the patrol's own ticker when the patrol is
// active, and returns a nil channel and stop func when it is not: a town that
// names no key starts nothing.
func (d *Daemon) startImagePinLayersTicker() (<-chan time.Time, func()) {
	if !d.isPatrolActive(imagePinLayersPatrol) {
		return nil, nil
	}
	newTicker := d.newImagePinLayersTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	ticks, stop := newTicker(imagePinLayersCheckTick)
	d.logger.Printf("Image pin layers ticker started (check interval %v)", imagePinLayersCheckTick)
	return ticks, stop
}

// startImagePinLayersTick runs one check on its own goroutine: a due run reads
// every pin from its registry, which does not belong on the main select loop. A
// tick that arrives while the previous one still runs is skipped and logged,
// since two runs at once would read the same registries twice.
func (d *Daemon) startImagePinLayersTick() {
	if d.config == nil || !d.isPatrolActive(imagePinLayersPatrol) {
		return
	}
	if !d.imagePinLayersRunning.CompareAndSwap(false, true) {
		d.logger.Printf("image_pin_layers: previous run still going, skipping")
		return
	}
	d.imagePinLayersCycles.Add(1)
	go func() {
		defer d.imagePinLayersCycles.Done()
		defer d.imagePinLayersRunning.Store(false)
		d.triggerImagePinLayers()
	}()
}

// triggerImagePinLayers asks the record whether today's run is due and logs what
// it did. A failed run is logged, never recorded as done: the next tick in the
// day runs it again.
func (d *Daemon) triggerImagePinLayers() {
	if d.config == nil || !d.isPatrolActive(imagePinLayersPatrol) {
		return
	}
	ran, note, err := d.runImagePinLayersTick(d.clk().Now())
	if note != "" {
		d.logger.Printf("image_pin_layers: %s", note)
	}
	if err != nil {
		d.logger.Printf("image_pin_layers: %v", err)
		return
	}
	if ran {
		d.logger.Printf("image_pin_layers: today's run completed")
	}
}

// runImagePinLayersTick is one tick: it asks whether at falls on a day the town
// has not run in, and calls the run seam only when it does, so a tick inside the
// day reads nothing. A run that cannot record its day is an error: without the
// record the next tick reads the day as due and runs the check again.
func (d *Daemon) runImagePinLayersTick(at time.Time) (bool, string, error) {
	record := imagePinLayersLastRunPath(d.config.TownRoot)
	decision := imagePinLayersDue(record, at)
	if !decision.due {
		return false, decision.note, nil
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, imagePinLayersTickTimeout)
	defer cancel()
	run := d.imagePinLayersRun
	if run == nil {
		run = imagePinLayersRunSkeleton
	}
	if err := run(ctx, at); err != nil {
		return false, decision.note, err
	}
	if err := writeImagePinLayersLastRun(record, at); err != nil {
		return true, decision.note, fmt.Errorf("recording the image pin layers run: %w", err)
	}
	return true, decision.note, nil
}

// stopImagePinLayersPatrol stops the patrol's ticker and joins the run it may
// have going, bounded, so Run's return leaves no run reading the town after the
// daemon is gone.
func (d *Daemon) stopImagePinLayersPatrol(stop func()) {
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		d.imagePinLayersCycles.Wait()
		close(done)
	}()
	bound := d.imagePinLayersJoinWait
	if bound <= 0 {
		bound = imagePinLayersJoinWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("image_pin_layers: a run is still going %s after the ticker stopped; Run leaves without it", bound)
	}
}
