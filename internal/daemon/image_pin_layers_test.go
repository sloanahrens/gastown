package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
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

// imagePinDurabilityDigest is the sha256 digest the fake registry names for seed.
func imagePinDurabilityDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// imagePinDurabilityPins is the test's own pin list: two host-qualified digest
// pins, so no test depends on the town's production pins.
func imagePinDurabilityPins() []string {
	return []string{
		"registry.example.com/app@" + imagePinDurabilityDigest("pinned app"),
		"registry.example.com/tool@" + imagePinDurabilityDigest("pinned tool"),
	}
}

// imagePinDurabilityPinDigest is the digest part of a pinned reference.
func imagePinDurabilityPinDigest(pinned string) string {
	_, digest, _ := strings.Cut(pinned, "@")
	return digest
}

// imagePinDurabilityManifest is an OCI image manifest naming one layer, the
// shape pincheck's readManifest parses. pincheck exports no manifest
// constructor, so the test writes the three fields that parser reads.
func imagePinDurabilityManifest(layer string) ([]byte, string, error) {
	const mediaType = "application/vnd.oci.image.manifest.v1+json"
	type descriptor struct {
		Digest string `json:"digest"`
	}
	body, err := json.Marshal(struct {
		MediaType string       `json:"mediaType"`
		Config    descriptor   `json:"config"`
		Layers    []descriptor `json:"layers"`
	}{
		MediaType: mediaType,
		Config:    descriptor{Digest: imagePinDurabilityDigest("config")},
		Layers:    []descriptor{{Digest: layer}},
	})
	return body, mediaType, err
}

// imagePinDurabilityRegistry is the fake registry: an image manifest with one
// served layer for every reference, the dead layer the pin's digest names in
// dead, and no answer when fail is set.
type imagePinDurabilityRegistry struct {
	asked []string
	dead  map[string]string
	fail  error
}

func (r *imagePinDurabilityRegistry) Manifest(_ context.Context, _, ref string) ([]byte, string, error) {
	r.asked = append(r.asked, ref)
	if r.fail != nil {
		return nil, "", r.fail
	}
	// pincheck asks for a manifest by the pinned digest, which is the key
	// dead names.
	layer := imagePinDurabilityDigest("served")
	if dead, ok := r.dead[ref]; ok {
		layer = dead
	}
	return imagePinDurabilityManifest(layer)
}

func (r *imagePinDurabilityRegistry) BlobExists(_ context.Context, _, digest string) (bool, error) {
	served := true
	for _, dead := range r.dead {
		served = served && dead != digest
	}
	return served, nil
}

// TestImagePinDurabilityARunRaisesAndAWholeRunClearsTheDeadLayerItem is the
// bead's first acceptance: a registry missing one layer makes a run raise
// KindImagePinsDeadLayer naming the image and the layer, and a whole run clears it.
func TestImagePinDurabilityARunRaisesAndAWholeRunClearsTheDeadLayerItem(t *testing.T) {
	t.Parallel()
	pins := imagePinDurabilityPins()
	town := t.TempDir()
	record := imagePinLayersFindingsPath(town)
	dead := imagePinDurabilityDigest("dead layer")
	broken := &imagePinDurabilityRegistry{dead: map[string]string{imagePinDurabilityPinDigest(pins[0]): dead}}
	var logged bytes.Buffer
	d := imagePinDurabilityTestDaemon(t, town, &logged)
	d.imagePinLayersRun = func(ctx context.Context, _ time.Time) error {
		return runImagePinLayers(ctx, record, pins, broken)
	}

	d.startImagePinLayersTick()
	d.imagePinLayersCycles.Wait()

	digests := []string{imagePinDurabilityPinDigest(pins[0]), imagePinDurabilityPinDigest(pins[1])}
	if !slices.Equal(broken.asked, digests) {
		t.Errorf("the run read %q, want every pin's digest %q", broken.asked, digests)
	}
	items, err := imagePinLayersObserved(record)
	if err != nil || len(items) != 1 {
		t.Fatalf("the run raised %d items (%v), want one for the pin with the dead layer", len(items), err)
	}
	item := items[0]
	if item.Kind != attention.KindImagePinsDeadLayer || !strings.Contains(item.Key, pins[0]) {
		t.Errorf("the item = %+v, want kind %q keyed by the dead pin %s", item, attention.KindImagePinsDeadLayer, pins[0])
	}
	if !strings.Contains(item.Summary, pins[0]) || !strings.Contains(item.Summary, dead) {
		t.Errorf("the item's summary = %q, want the image %s and the dead digest %s named", item.Summary, pins[0], dead)
	}
	if !strings.Contains(logged.String(), item.Key) || !strings.Contains(logged.String(), dead) {
		t.Errorf("the tick logged %q, want the raised item %q named", logged.String(), item.Key)
	}

	whole := &imagePinDurabilityRegistry{}
	d.imagePinLayersRun = func(ctx context.Context, _ time.Time) error {
		return runImagePinLayers(ctx, record, pins, whole)
	}
	if ran, _, err := d.runImagePinLayersTick(time.Now().AddDate(0, 0, 1)); err != nil || !ran {
		t.Fatalf("the next day's run = %v, %v; want a completed run", ran, err)
	}
	items, err = imagePinLayersObserved(record)
	if err != nil {
		t.Fatalf("reading the record after the whole run: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("a run over a whole registry left %d items standing, want the item cleared", len(items))
	}
}

// TestImagePinDurabilityTheRegisteredCollectorReturnsTheRecordedItem is the
// bead's third acceptance: the attention queue's collector set holds a
// KindImagePinsDeadLayer collector that returns the item a run recorded, so a
// finding reaches the queue.
func TestImagePinDurabilityTheRegisteredCollectorReturnsTheRecordedItem(t *testing.T) {
	t.Parallel()
	pins := imagePinDurabilityPins()
	town := t.TempDir()
	record := imagePinLayersFindingsPath(town)
	dead := imagePinDurabilityDigest("dead layer")
	// Both pins are dead, so the collector's order is the one it sorts to.
	broken := &imagePinDurabilityRegistry{dead: map[string]string{
		imagePinDurabilityPinDigest(pins[0]): dead,
		imagePinDurabilityPinDigest(pins[1]): imagePinDurabilityDigest("dead layer two"),
	}}
	if err := runImagePinLayers(context.Background(), record, pins, broken); err != nil {
		t.Fatalf("the run: %v", err)
	}

	d := imagePinDurabilityTestDaemon(t, town, io.Discard)
	var collectors []attentionCollector
	for _, c := range d.attentionSources(time.Now()).collectors() {
		if c.kind == attention.KindImagePinsDeadLayer {
			collectors = append(collectors, c)
		}
	}
	if len(collectors) != 1 {
		t.Fatalf("the attention queue has %d %q collectors, want one", len(collectors), attention.KindImagePinsDeadLayer)
	}
	items, err := collectors[0].collect(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("the registered collector returned %d items (%v), want the two the run recorded", len(items), err)
	}
	if !strings.Contains(items[0].Key, pins[0]) || !strings.Contains(items[1].Key, pins[1]) {
		t.Errorf("the collector returned %q, want the pins in key order, %s then %s", []string{items[0].Key, items[1].Key}, pins[0], pins[1])
	}
	for _, item := range items {
		if item.Kind != attention.KindImagePinsDeadLayer {
			t.Errorf("the collector returned kind %q, want %q", item.Kind, attention.KindImagePinsDeadLayer)
		}
	}
	if !strings.Contains(items[0].Summary, dead) {
		t.Errorf("the collector's summary = %q, want the dead digest %s named", items[0].Summary, dead)
	}
}

// TestImagePinDurabilityAnOffPatrolRaisesNoDeadLayerItem: the queue's collector
// has no record to read while the patrol is off, so it returns no items.
func TestImagePinDurabilityAnOffPatrolRaisesNoDeadLayerItem(t *testing.T) {
	t.Parallel()
	s := &attentionSources{}
	items, err := s.collectImagePinLayersDeadLayer(context.Background())
	if err != nil || len(items) != 0 {
		t.Errorf("the collector with the patrol off returned %d items (%v), want none", len(items), err)
	}
}

// TestImagePinDurabilityARecordThatWillNotOpenOrWriteIsAnError: an unreadable
// record is an error, never an empty registry, and an unwritable one fails the run.
func TestImagePinDurabilityARecordThatWillNotOpenOrWriteIsAnError(t *testing.T) {
	t.Parallel()
	corrupt := filepath.Join(t.TempDir(), imagePinLayersFindingsFile)
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write a corrupt record: %v", err)
	}
	if _, err := imagePinLayersObserved(corrupt); err == nil || !strings.Contains(err.Error(), "parsing the dead-layer record") {
		t.Fatalf("reading a corrupt record = %v, want the parse error", err)
	}
	held := filepath.Join(t.TempDir(), imagePinLayersFindingsFile)
	if err := os.MkdirAll(held, 0o755); err != nil {
		t.Fatalf("hold the record's path with a directory: %v", err)
	}
	if _, err := imagePinLayersObserved(held); err == nil || !strings.Contains(err.Error(), "reading the dead-layer record") {
		t.Fatalf("reading a record held by a directory = %v, want the read error", err)
	}
	if err := runImagePinLayers(context.Background(), held, imagePinDurabilityPins(), &imagePinDurabilityRegistry{}); err == nil || !strings.Contains(err.Error(), "recording the image pin layers findings") {
		t.Fatalf("a run over an unwritable record = %v, want the record named", err)
	}
}

// TestImagePinDurabilityAHostlessRepositoryIsReadAtDockerHub: a pin naming no
// registry host is read at Docker Hub's library, so it is addressable.
func TestImagePinDurabilityAHostlessRepositoryIsReadAtDockerHub(t *testing.T) {
	t.Parallel()
	digest := imagePinDurabilityDigest("pin")
	if got, want := imagePinLayersRegistryRef("alpine:3@"+digest), imagePinLayersHubHost+"/library/alpine@"+digest; got != want {
		t.Errorf("a hostless repository was read at %q, want %q", got, want)
	}
}

// TestImagePinDurabilityAnUnreachableRegistryRaisesNothingAndIsLogged is the
// bead's second acceptance: an unreachable registry raises no item and is logged.
func TestImagePinDurabilityAnUnreachableRegistryRaisesNothingAndIsLogged(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	record := imagePinLayersFindingsPath(town)
	refused := &imagePinDurabilityRegistry{fail: errors.New("dial tcp: the registry is unreachable")}
	var logged bytes.Buffer
	d := imagePinDurabilityTestDaemon(t, town, &logged)
	d.imagePinLayersRun = func(ctx context.Context, _ time.Time) error {
		return runImagePinLayers(ctx, record, imagePinDurabilityPins(), refused)
	}

	d.startImagePinLayersTick()
	d.imagePinLayersCycles.Wait()

	if !strings.Contains(logged.String(), "the registry is unreachable") {
		t.Errorf("the tick logged %q, want the failed check named", logged.String())
	}
	if strings.Contains(logged.String(), imagePinLayersDeadLayerKey) {
		t.Errorf("an unreachable registry logged %q, want no dead-layer item", logged.String())
	}
	if items, err := imagePinLayersObserved(record); err != nil || len(items) != 0 {
		t.Errorf("an unreachable registry left %d items (%v), want none raised", len(items), err)
	}
	if _, err := os.Stat(imagePinLayersLastRunPath(town)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an unreachable registry's tick wrote a last-run record (%v), want the day left due", err)
	}
}

// TestImagePinDurabilityTheStartedTickerInstallsTheProductionRun: a started
// ticker gives a town with no seam the production check; a patrol off installs none.
func TestImagePinDurabilityTheStartedTickerInstallsTheProductionRun(t *testing.T) {
	t.Parallel()
	newTicker := func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	on := imagePinDurabilityTestDaemon(t, t.TempDir(), io.Discard)
	on.newImagePinLayersTicker = newTicker
	if _, stop := on.startImagePinLayersTicker(); stop == nil {
		t.Fatal("the enabled patrol started no ticker")
	}
	if on.imagePinLayersRun == nil {
		t.Error("the started ticker left no run seam, so a tick would run nothing")
	}
	off := imagePinDurabilityTestDaemon(t, t.TempDir(), io.Discard)
	off.newImagePinLayersTicker, off.patrolConfig = newTicker, &DaemonPatrolConfig{}
	if _, stop := off.startImagePinLayersTicker(); stop != nil || off.imagePinLayersRun != nil {
		t.Error("a patrol the town turned off started a ticker or installed a run")
	}
}
