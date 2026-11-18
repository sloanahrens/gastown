package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// imagePinDurabilityNoon is noon UTC on day, so a record's day is that date in
// every zone the record could be read in.
func imagePinDurabilityNoon(t *testing.T, day string) time.Time {
	t.Helper()
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatalf("test day %q: %v", day, err)
	}
	return at.Add(12 * time.Hour)
}

// imagePinDurabilityTestDaemon builds a daemon whose image_pin_layers patrol is
// enabled.
func imagePinDurabilityTestDaemon(t *testing.T, town string, logged io.Writer) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ImagePinLayers: &agentconfig.ImagePinLayersConfig{Enabled: true},
		}},
	}
}

// imagePinDurabilityCountingDaemon builds an enabled patrol whose run seam
// counts its runs, over the town the caller gives so a fresh value can read
// what an earlier one recorded.
func imagePinDurabilityCountingDaemon(t *testing.T, town string, calls *atomic.Int32, logged io.Writer) *Daemon {
	t.Helper()
	d := imagePinDurabilityTestDaemon(t, town, logged)
	d.imagePinLayersRun = func(context.Context, time.Time) error {
		calls.Add(1)
		return nil
	}
	return d
}

// TestImagePinDurabilityImagePinLayersPatrolStartsOnlyWhenEnabled is the bead's
// first acceptance: the patrol's own ticker exists only when
// daemon.patrols.image_pin_layers.enabled is true, and a town that names no key
// starts nothing.
func TestImagePinDurabilityImagePinLayersPatrolStartsOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	var intervals []time.Duration
	ticks := make(chan time.Time)
	stops := 0
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
	d.newImagePinLayersTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}

	for _, tt := range []struct {
		name       string
		config     *DaemonPatrolConfig
		wantTicker bool
	}{
		{"no block", nil, false},
		{"a disabled block", &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ImagePinLayers: &agentconfig.ImagePinLayersConfig{Enabled: false},
		}}, false},
		{"an enabled block", &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ImagePinLayers: &agentconfig.ImagePinLayersConfig{Enabled: true},
		}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(intervals)
			d.patrolConfig = tt.config
			channel, stop := d.startImagePinLayersTicker()
			if !tt.wantTicker {
				if channel != nil || stop != nil {
					t.Errorf("%s started a ticker, want none", tt.name)
				}
				if len(intervals) != before {
					t.Errorf("%s asked for a ticker at %v, want none", tt.name, intervals[before:])
				}
				return
			}
			if channel == nil || stop == nil {
				t.Fatalf("%s started no ticker", tt.name)
			}
			if len(intervals) != before+1 || intervals[before] != imagePinLayersCheckTick {
				t.Errorf("%s asked for tickers at %v, want one at the %v check cadence", tt.name, intervals[before:], imagePinLayersCheckTick)
			}
			stop()
			if stops != 1 {
				t.Errorf("%s ran the stop func %d times, want the ticker stopped once", tt.name, stops)
			}
		})
	}
}

// TestImagePinDurabilityImagePinLayersSecondRunInsideTheDayIsSkipped is the
// bead's second acceptance: a tick in the day the last-run record names does
// nothing, and it is the town's record that decides, so a freshly built patrol
// — as a restarted daemon builds — decides as the first did.
func TestImagePinDurabilityImagePinLayersSecondRunInsideTheDayIsSkipped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	base := imagePinDurabilityNoon(t, "2026-11-16")
	var calls atomic.Int32

	first := imagePinDurabilityCountingDaemon(t, town, &calls, io.Discard)
	ran, _, err := first.runImagePinLayersTick(base)
	if err != nil || !ran {
		t.Fatalf("the day's first tick = %v, %v; want a run with no error", ran, err)
	}

	ran, note, err := first.runImagePinLayersTick(base.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("the second tick inside the day: %v", err)
	}
	if ran {
		t.Error("a second tick inside the day ran, want it to do nothing")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a second tick inside the day called the run seam %d times, want the first tick's one", got)
	}
	if !strings.Contains(note, "already ran") {
		t.Errorf("the second tick said %q, want the day named as already run", note)
	}

	restarted := imagePinDurabilityCountingDaemon(t, town, &calls, io.Discard)
	ran, note, err = restarted.runImagePinLayersTick(base.Add(6 * time.Hour))
	if err != nil {
		t.Fatalf("the restarted patrol's tick inside the day: %v", err)
	}
	if ran {
		t.Error("a restarted patrol ran again inside the day, want the persisted record to hold")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a restarted patrol called the run seam %d times, want the persisted record to hold", got)
	}
	if !strings.Contains(note, "already ran") {
		t.Errorf("a restarted patrol said %q, want the day named as already run", note)
	}
}

// TestImagePinDurabilityImagePinLayersNextDayRunsAgain: the record holds the
// day, not a countdown, so the next day's first tick runs the seam again.
func TestImagePinDurabilityImagePinLayersNextDayRunsAgain(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	base := imagePinDurabilityNoon(t, "2026-11-16")
	var calls atomic.Int32
	d := imagePinDurabilityCountingDaemon(t, town, &calls, io.Discard)

	if ran, _, err := d.runImagePinLayersTick(base); err != nil || !ran {
		t.Fatalf("the first day's tick = %v, %v; want a run with no error", ran, err)
	}
	ran, note, err := d.runImagePinLayersTick(base.AddDate(0, 0, 1))
	if err != nil || !ran {
		t.Fatalf("the next day's tick = %v, %v; want a run with no error", ran, err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the two days called the run seam %d times, want one run each", got)
	}
	if !strings.Contains(note, "is due") {
		t.Errorf("the next day's tick said %q, want the new day named as due", note)
	}
}

// TestImagePinDurabilityImagePinLayersUnreadableRecordRunsAndSaysSo: a record
// the town cannot read makes today due and says why, rather than skipping the
// check on a broken file.
func TestImagePinDurabilityImagePinLayersUnreadableRecordRunsAndSaysSo(t *testing.T) {
	t.Parallel()
	base := imagePinDurabilityNoon(t, "2026-11-16")

	for _, tt := range []struct {
		name     string
		prepare  func(t *testing.T, record string)
		wantNote string
	}{
		{
			name:     "no record",
			prepare:  func(*testing.T, string) {},
			wantNote: "no last-run record",
		},
		{
			name: "a corrupt record",
			prepare: func(t *testing.T, record string) {
				t.Helper()
				if err := os.WriteFile(record, []byte("{not json"), 0o644); err != nil {
					t.Fatalf("write a corrupt last-run record: %v", err)
				}
			},
			wantNote: "cannot be read",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			record := imagePinLayersLastRunPath(town)
			if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
				t.Fatalf("make the record's directory: %v", err)
			}
			tt.prepare(t, record)

			var calls atomic.Int32
			d := imagePinDurabilityCountingDaemon(t, town, &calls, io.Discard)
			ran, note, err := d.runImagePinLayersTick(base)
			if err != nil || !ran {
				t.Fatalf("a tick over %s = %v, %v; want a run with no error", tt.name, ran, err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("a tick over %s called the run seam %d times, want the run", tt.name, got)
			}
			if !strings.Contains(note, tt.wantNote) {
				t.Errorf("a tick over %s said %q, want %q", tt.name, note, tt.wantNote)
			}
		})
	}
}

// TestImagePinDurabilityImagePinLayersRecordItCannotWriteIsAnError: the run
// happens, and the tick still fails, naming the record: without the record the
// next tick reads the day as due and runs the check again. A record path held by
// a directory is both unreadable and unwritable, which drives the read's own
// error branch as well.
func TestImagePinDurabilityImagePinLayersRecordItCannotWriteIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	record := imagePinLayersLastRunPath(town)
	if err := os.MkdirAll(record, 0o755); err != nil {
		t.Fatalf("hold the record's path with a directory: %v", err)
	}
	var calls atomic.Int32
	d := imagePinDurabilityCountingDaemon(t, town, &calls, io.Discard)

	ran, note, err := d.runImagePinLayersTick(imagePinDurabilityNoon(t, "2026-11-16"))
	if !ran {
		t.Fatalf("a tick over an unwritable record = %v, %v; want the run to happen", ran, err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a tick over an unreadable record called the run seam %d times, want the run", got)
	}
	if !strings.Contains(note, "cannot be read") {
		t.Errorf("a tick over an unreadable record said %q, want the read failure named", note)
	}
	if err == nil || !strings.Contains(err.Error(), "recording the image pin layers run") {
		t.Fatalf("a tick over an unwritable record = %v, want the record named", err)
	}
}

// TestImagePinDurabilityImagePinLayersNoRunSeamRunsNothing: this build installs
// no run seam, so a tick with none runs nothing, reports no completed run and
// records no day: the next tick still reads the day as due.
func TestImagePinDurabilityImagePinLayersNoRunSeamRunsNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var logged bytes.Buffer
	d := imagePinDurabilityTestDaemon(t, town, &logged)

	ran, note, err := d.runImagePinLayersTick(imagePinDurabilityNoon(t, "2026-11-16"))
	if ran || err != nil {
		t.Fatalf("a tick with no run seam = %v, %v; want no run and no error", ran, err)
	}
	if !strings.Contains(note, "nothing ran") {
		t.Errorf("a tick with no run seam said %q, want it to say nothing ran", note)
	}

	d.startImagePinLayersTick()
	d.imagePinLayersCycles.Wait()

	if strings.Contains(logged.String(), "today's run completed") {
		t.Errorf("a tick with no run seam logged a completed run: %q", logged.String())
	}
	if !strings.Contains(logged.String(), "nothing ran") {
		t.Errorf("a tick with no run seam logged %q, want it to say nothing ran", logged.String())
	}
	if _, err := os.Stat(imagePinLayersLastRunPath(town)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a tick with no run seam wrote a last-run record (%v), want none so the day stays due", err)
	}
}

// TestImagePinDurabilityImagePinLayersTickRunsTheSeamOnce: one started tick
// calls the run seam once and frees the guard; a tick while a run is going is
// skipped and logged; and a failed run is logged, not recorded as done.
func TestImagePinDurabilityImagePinLayersTickRunsTheSeamOnce(t *testing.T) {
	t.Parallel()

	t.Run("one tick, one run", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		var calls atomic.Int32
		d := imagePinDurabilityCountingDaemon(t, t.TempDir(), &calls, &logged)

		d.startImagePinLayersTick()
		d.imagePinLayersCycles.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("a tick called the run seam %d times, want once", got)
		}
		if d.imagePinLayersRunning.Load() {
			t.Error("the guard stayed set after the run ended, so no later tick could run")
		}
		if !strings.Contains(logged.String(), "today's run completed") {
			t.Errorf("the tick logged %q, want the run it completed", logged.String())
		}
	})

	t.Run("a tick while one runs is skipped", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var calls atomic.Int32
		d := imagePinDurabilityTestDaemon(t, t.TempDir(), &logged)
		d.imagePinLayersRun = func(context.Context, time.Time) error {
			calls.Add(1)
			reached <- struct{}{}
			<-release
			return nil
		}

		d.startImagePinLayersTick()
		waitForSignal(t, reached, "the first run never reached the run seam")
		d.startImagePinLayersTick()
		if !strings.Contains(logged.String(), "previous run still going, skipping") {
			t.Errorf("the second tick was not logged as skipped: %q", logged.String())
		}
		close(release)
		d.imagePinLayersCycles.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("two starting ticks called the run seam %d times, want once", got)
		}
	})

	t.Run("a failed run is logged and not recorded", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		town := t.TempDir()
		var calls atomic.Int32
		d := imagePinDurabilityTestDaemon(t, town, &logged)
		d.imagePinLayersRun = func(context.Context, time.Time) error {
			calls.Add(1)
			return errors.New("the registry is unreachable")
		}

		d.startImagePinLayersTick()
		d.imagePinLayersCycles.Wait()

		if !strings.Contains(logged.String(), "the registry is unreachable") {
			t.Errorf("a failed run logged %q, want the error named", logged.String())
		}
		if strings.Contains(logged.String(), "today's run completed") {
			t.Errorf("a failed run logged a completed run: %q", logged.String())
		}
		if _, err := os.Stat(imagePinLayersLastRunPath(town)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a failed run wrote a last-run record (%v), want none so the next tick retries", err)
		}

		d.startImagePinLayersTick()
		d.imagePinLayersCycles.Wait()
		if got := calls.Load(); got != 2 {
			t.Errorf("the tick after a failed run called the run seam %d times, want the retry", got)
		}
	})
}

// TestImagePinDurabilityImagePinLayersTickOnInactivePatrolRunsNothing: a tick
// can outlive the reload that turned the patrol off, and a patrol the town
// turned off calls no seam and prints no diagnosis.
func TestImagePinDurabilityImagePinLayersTickOnInactivePatrolRunsNothing(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	var calls atomic.Int32
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logged, "", 0),
	}
	d.imagePinLayersRun = func(context.Context, time.Time) error {
		calls.Add(1)
		return nil
	}

	d.startImagePinLayersTick()
	d.imagePinLayersCycles.Wait()

	if got := calls.Load(); got != 0 {
		t.Errorf("a tick on an inactive patrol called the run seam %d times, want none", got)
	}
	if logged.Len() != 0 {
		t.Errorf("a tick on an inactive patrol logged %q, want nothing", logged.String())
	}
}

// TestImagePinDurabilityImagePinLayersStopJoinsTheRun: Run's return stops the
// ticker and waits for the run it started, and a run still going at the bound is
// logged and left.
func TestImagePinDurabilityImagePinLayersStopJoinsTheRun(t *testing.T) {
	t.Parallel()

	blocking := func(t *testing.T, logged io.Writer) (*Daemon, chan struct{}, func()) {
		t.Helper()
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		d := imagePinDurabilityTestDaemon(t, t.TempDir(), logged)
		d.imagePinLayersRun = func(context.Context, time.Time) error {
			reached <- struct{}{}
			<-release
			return nil
		}
		t.Cleanup(func() {
			unblock()
			d.imagePinLayersCycles.Wait()
		})
		return d, reached, unblock
	}

	t.Run("the run the ticker started is joined", func(t *testing.T) {
		t.Parallel()
		d, reached, unblock := blocking(t, io.Discard)
		held := make(chan time.Time)
		d.imagePinLayersJoinTimer = func(time.Duration) (<-chan time.Time, func()) {
			return held, func() {}
		}

		d.startImagePinLayersTick()
		waitForSignal(t, reached, "the run never reached the run seam")

		stopped := make(chan struct{})
		stopCalled := make(chan struct{})
		go func() {
			d.stopImagePinLayersPatrol(func() { close(stopCalled) })
			close(stopped)
		}()
		waitForSignal(t, stopCalled, "the stop func never ran")
		select {
		case <-stopped:
			t.Fatal("stopImagePinLayersPatrol returned while the run still went")
		default:
		}
		unblock()
		waitForSignal(t, stopped, "stopImagePinLayersPatrol never returned after the run ended")
		if d.imagePinLayersRunning.Load() {
			t.Error("the join returned with the run's guard still set")
		}
	})

	t.Run("a run past the bound is logged and left", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d, reached, unblock := blocking(t, &logged)
		expired := make(chan time.Time)
		var gotBound atomic.Int64
		var boundStopped atomic.Int32
		d.imagePinLayersJoinTimer = func(wait time.Duration) (<-chan time.Time, func()) {
			gotBound.Store(int64(wait))
			return expired, func() { boundStopped.Add(1) }
		}

		d.startImagePinLayersTick()
		waitForSignal(t, reached, "the run never reached the run seam")

		stopped := make(chan struct{})
		go func() {
			d.stopImagePinLayersPatrol(nil)
			close(stopped)
		}()
		close(expired)
		waitForSignal(t, stopped, "stopImagePinLayersPatrol never returned past the bound")
		if !strings.Contains(logged.String(), "still going") {
			t.Errorf("a run left past the bound was not logged: %q", logged.String())
		}
		if got := time.Duration(gotBound.Load()); got != imagePinLayersJoinWait {
			t.Errorf("the bound's timer was started for %v, want the %v join bound", got, imagePinLayersJoinWait)
		}
		if got := boundStopped.Load(); got != 1 {
			t.Errorf("the bound's timer was stopped %d times, want once", got)
		}
		unblock()
		d.imagePinLayersCycles.Wait()
	})
}
