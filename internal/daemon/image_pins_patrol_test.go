package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/daemonimage"
)

// tickPinsFile is the pins file the tick's reader answers, so a test can check
// the tip reader and the planner see those bytes unchanged.
var tickPinsFile = []byte("bd https://forge.example.com/town/bd " + pinBD)

// tickCounts counts each seam one tick called, so a test shows which read the
// tick stopped at.
type tickCounts struct {
	pins, tips, open, plan, file int
}

// tickSeams wires a full tick over the given answers, filing through store and
// asking the production planner.
func tickSeams(counts *tickCounts, now time.Time, pins []ImagePinState, open *ImagePinRefreshBead, store *refreshStore) imagePinsTickSeams {
	return imagePinsTickSeams{
		now: func() time.Time {
			return now
		},
		readPins: func() ([]byte, error) {
			counts.pins++
			return tickPinsFile, nil
		},
		readTips: func(_ context.Context, _ []byte) ([]ImagePinState, error) {
			counts.tips++
			return pins, nil
		},
		openBead: func() (*ImagePinRefreshBead, error) {
			counts.open++
			return open, nil
		},
		file: func(pins []ImagePinState) (string, error) {
			counts.file++
			return imagePinsFileRefreshBead(store.Create, pins)
		},
	}
}

// tickItemNames renders each item as "key/pin", the pin empty for the item that
// is about the open bead rather than a pin.
func tickItemNames(items []ImagePinItem) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Key + "/" + item.Pin
	}
	return out
}

// TestImagePinsTick is the tick's decision table: the bead it files and the
// planner's items it returns for each distance from the tip and each state of
// the open refresh bead.
func TestImagePinsTick(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	// A failed read's Behind is not trusted: the state carries a stale count so
	// the row fails if the error is ignored.
	failed := ImagePinState{Name: "om", Pin: pinOM, Behind: 34, Err: errors.New("ls-remote exited 128")}
	tests := []struct {
		name      string
		pins      []ImagePinState
		open      *ImagePinRefreshBead
		wantID    string
		wantTitle string
		wantItems []string
	}{
		{
			name: "a pin at its source tip files nothing",
			pins: []ImagePinState{refreshPin("bd", pinBD, tipBD, 0)},
		},
		{
			name: "a pin one commit short of the trigger files nothing",
			pins: []ImagePinState{refreshPin("bd", pinBD, tipBD, 19)},
		},
		{
			name:      "a pin 20 behind files one bead",
			pins:      []ImagePinState{refreshPin("bd", pinBD, tipBD, 20)},
			wantID:    "gt-refresh9",
			wantTitle: "bd by 20 commits",
		},
		{
			name:      "a stale pin files and its stale item comes back",
			pins:      []ImagePinState{refreshPin("bd", pinBD, tipBD, 30)},
			wantID:    "gt-refresh9",
			wantTitle: "bd by 30 commits",
			wantItems: []string{imagePinsStaleKey + "/bd"},
		},
		{
			name:      "a failed read comes back as an unreadable item and files nothing",
			pins:      []ImagePinState{failed},
			wantItems: []string{imagePinsUnreadableKey + "/om"},
		},
		{
			name: "an open refresh bead files nothing",
			pins: []ImagePinState{refreshPin("bd", pinBD, tipBD, 20)},
			open: &ImagePinRefreshBead{ID: "gt-refresh4", OpenedAt: now.Add(-time.Hour)},
		},
		{
			name:      "a stuck open bead comes back as an item",
			pins:      []ImagePinState{refreshPin("bd", pinBD, tipBD, 20)},
			open:      &ImagePinRefreshBead{ID: "gt-refresh4", OpenedAt: now.Add(-4 * time.Hour)},
			wantItems: []string{imagePinsRefreshStuckKey + "/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counts := &tickCounts{}
			store := &refreshStore{}
			gotID, items, err := runImagePinsTick(context.Background(), tickSeams(counts, now, tt.pins, tt.open, store))
			if err != nil {
				t.Fatalf("runImagePinsTick: %v", err)
			}
			if gotID != tt.wantID {
				t.Errorf("filed id = %q, want %q", gotID, tt.wantID)
			}
			wantFiles := 0
			if tt.wantID != "" {
				wantFiles = 1
			}
			if got := len(store.created); got != wantFiles {
				t.Errorf("beads filed = %d, want %d", got, wantFiles)
			}
			if tt.wantTitle != "" {
				if len(store.created) == 0 {
					t.Fatalf("no bead filed, want one whose title says %q", tt.wantTitle)
				}
				if title := store.created[0].Title; !strings.Contains(title, tt.wantTitle) {
					t.Errorf("filed title %q does not say %q", title, tt.wantTitle)
				}
				if labels := store.created[0].Labels; !slices.Contains(labels, imagePinsRefreshLabel) {
					t.Errorf("filed labels = %v, want %s among them", labels, imagePinsRefreshLabel)
				}
			}
			if got := tickItemNames(items); !slices.Equal(got, tt.wantItems) {
				t.Errorf("items = %v, want %v", got, tt.wantItems)
			}
			if counts.pins != 1 || counts.tips != 1 || counts.open != 1 || counts.file != wantFiles {
				t.Errorf("seam calls (pins, tips, open, plan, file) = (%d, %d, %d, %d, %d), want (1, 1, 1, 0, %d)",
					counts.pins, counts.tips, counts.open, counts.plan, counts.file, wantFiles)
			}
		})
	}
}

// TestImagePinsTickAsksThePlanner: the planner reads the bytes the pins reader
// answered, the states the tip reader returned, the open bead the lookup found
// and the injected clock, so a decision is made against what the town has now.
func TestImagePinsTickAsksThePlanner(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	pins := []ImagePinState{refreshPin("bd", pinBD, tipBD, 20)}
	open := &ImagePinRefreshBead{ID: "gt-refresh4", OpenedAt: now.Add(-time.Hour)}
	var gotTipsData []byte
	var gotPins []ImagePinState
	var gotOpen *ImagePinRefreshBead
	var gotNow time.Time
	seams := imagePinsTickSeams{
		now:      func() time.Time { return now },
		readPins: func() ([]byte, error) { return tickPinsFile, nil },
		readTips: func(_ context.Context, data []byte) ([]ImagePinState, error) {
			gotTipsData = data
			return pins, nil
		},
		openBead: func() (*ImagePinRefreshBead, error) { return open, nil },
		plan: func(p []ImagePinState, o *ImagePinRefreshBead, n time.Time) ImagePinPlan {
			gotPins, gotOpen, gotNow = p, o, n
			return ImagePinPlan{}
		},
		file: func([]ImagePinState) (string, error) {
			t.Error("the filer ran for a plan that named no pin")
			return "", nil
		},
	}
	id, items, err := runImagePinsTick(context.Background(), seams)
	if err != nil {
		t.Fatalf("runImagePinsTick: %v", err)
	}
	if id != "" || len(items) != 0 {
		t.Errorf("filed id = %q, items = %v, want no bead and no item", id, tickItemNames(items))
	}
	if !slices.Equal(gotTipsData, tickPinsFile) {
		t.Errorf("tip reader got %q, want the pins reader's %q", gotTipsData, tickPinsFile)
	}
	if !slices.Equal(gotPins, pins) {
		t.Errorf("planner got pins %+v, want %+v", gotPins, pins)
	}
	if gotOpen != open {
		t.Errorf("planner got open bead %+v, want %+v", gotOpen, open)
	}
	if !gotNow.Equal(now) {
		t.Errorf("planner got clock %v, want %v", gotNow, now)
	}
}

// TestImagePinsTickRefusesUnwiredSeams: every seam the tick cannot run without
// is refused by name before the first read, so an unwired tick fails loudly
// instead of reporting the quiet "" that means nothing was due.
func TestImagePinsTickRefusesUnwiredSeams(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		unset func(*imagePinsTickSeams)
		want  string
	}{
		{"no clock", func(s *imagePinsTickSeams) { s.now = nil }, "clock"},
		{"no pins reader", func(s *imagePinsTickSeams) { s.readPins = nil }, "pins reader"},
		{"no tip reader", func(s *imagePinsTickSeams) { s.readTips = nil }, "tip reader"},
		{"no open refresh bead lookup", func(s *imagePinsTickSeams) { s.openBead = nil }, "open refresh bead lookup"},
		{"no filer", func(s *imagePinsTickSeams) { s.file = nil }, "filer"},
		{"every seam unset", func(s *imagePinsTickSeams) { *s = imagePinsTickSeams{} }, "clock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counts := &tickCounts{}
			seams := tickSeams(counts, now, []ImagePinState{refreshPin("bd", pinBD, tipBD, 20)}, nil, &refreshStore{})
			tt.unset(&seams)
			id, items, err := runImagePinsTick(context.Background(), seams)
			if err == nil {
				t.Fatalf("runImagePinsTick = (%q, %v, nil), want an error", id, tickItemNames(items))
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}
			if id != "" || items != nil {
				t.Errorf("refused tick = (%q, %v), want no bead and no items", id, items)
			}
			if *counts != (tickCounts{}) {
				t.Errorf("a refused tick called a seam: %+v", *counts)
			}
		})
	}
}

// TestImagePinsTickStopsAtTheFailedRead: each failed read or failed file ends
// the tick with an error and no bead id, before the reads below it run, and the
// items the planner raised before the failure still come back.
func TestImagePinsTickStopsAtTheFailedRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	stale := []ImagePinState{refreshPin("bd", pinBD, tipBD, 30)}
	tests := []struct {
		name      string
		pins      []ImagePinState
		pinsErr   error
		tipsErr   error
		openErr   error
		fileErr   error
		fileNoID  bool
		wantTips  int
		wantOpen  int
		wantFile  int
		wantItems []string
	}{
		{
			name:    "the pins file cannot be read",
			pinsErr: errors.New("open: permission denied"),
		},
		{
			name:     "the pins' source tips cannot be read",
			tipsErr:  errors.New("compare answered 502"),
			wantTips: 1,
		},
		{
			name:     "the pins read names no pin",
			wantTips: 1,
		},
		{
			name:     "the open refresh bead cannot be read",
			pins:     stale,
			openErr:  errors.New("bd list exited 1"),
			wantTips: 1,
			wantOpen: 1,
		},
		{
			name:      "the refresh bead cannot be filed",
			pins:      stale,
			fileErr:   errors.New("bd create exited 1"),
			wantTips:  1,
			wantOpen:  1,
			wantFile:  1,
			wantItems: []string{imagePinsStaleKey + "/bd"},
		},
		{
			name:      "the filer returns no bead id",
			pins:      stale,
			fileNoID:  true,
			wantTips:  1,
			wantOpen:  1,
			wantFile:  1,
			wantItems: []string{imagePinsStaleKey + "/bd"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counts := &tickCounts{}
			seams := imagePinsTickSeams{
				now: func() time.Time { return now },
				readPins: func() ([]byte, error) {
					counts.pins++
					if tt.pinsErr != nil {
						return nil, tt.pinsErr
					}
					return tickPinsFile, nil
				},
				readTips: func(_ context.Context, _ []byte) ([]ImagePinState, error) {
					counts.tips++
					if tt.tipsErr != nil {
						return nil, tt.tipsErr
					}
					return tt.pins, nil
				},
				openBead: func() (*ImagePinRefreshBead, error) {
					counts.open++
					if tt.openErr != nil {
						return nil, tt.openErr
					}
					return nil, nil
				},
				file: func([]ImagePinState) (string, error) {
					counts.file++
					if tt.fileErr != nil {
						return "", tt.fileErr
					}
					if tt.fileNoID {
						return "", nil
					}
					return "gt-refresh9", nil
				},
			}
			id, items, err := runImagePinsTick(context.Background(), seams)
			if err == nil {
				t.Fatalf("runImagePinsTick = (%q, %v, nil), want an error", id, tickItemNames(items))
			}
			if id != "" {
				t.Errorf("filed id = %q, want none", id)
			}
			if got := tickItemNames(items); !slices.Equal(got, tt.wantItems) {
				t.Errorf("items = %v, want %v", got, tt.wantItems)
			}
			if counts.tips != tt.wantTips || counts.open != tt.wantOpen || counts.file != tt.wantFile {
				t.Errorf("seam calls (tips, open, file) = (%d, %d, %d), want (%d, %d, %d)",
					counts.tips, counts.open, counts.file, tt.wantTips, tt.wantOpen, tt.wantFile)
			}
		})
	}
}

// TestImagePinsPatrolIsOffUnlessEnabled holds the opt-in default (gt-01lcq2): a
// town that never names image_pins runs no tick, and a town that names it runs
// one unless disabled_patrols holds it off.
func TestImagePinsPatrolIsOffUnlessEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *DaemonPatrolConfig
		want   bool
	}{
		{"no config", nil, false},
		{"no patrols section", &DaemonPatrolConfig{}, false},
		{"the key absent", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, false},
		{"enabled false", &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: false}}}, false},
		{"enabled true", &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true}}}, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPatrolEnabled(tt.config, "image_pins"); got != tt.want {
				t.Errorf("IsPatrolEnabled(image_pins) = %v, want %v", got, tt.want)
			}
		})
	}

	d := &Daemon{
		patrolConfig:    &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true}}},
		disabledPatrols: map[string]bool{"image_pins": true},
	}
	if d.isPatrolActive("image_pins") {
		t.Error("isPatrolActive says image_pins runs: a town that disabled it runs no tick")
	}
}

// TestImagePinsInterval: the ticker asks for the patrol's configured interval,
// clamped up to the floor, and for the default when the key is absent or does
// not parse.
func TestImagePinsInterval(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *DaemonPatrolConfig
		want   time.Duration
	}{
		{"no config", nil, defaultImagePinsInterval},
		{"the key absent", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, defaultImagePinsInterval},
		{"an interval that does not parse", &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true, Interval: "half an hour"}}}, defaultImagePinsInterval},
		{"a zero interval", &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true, Interval: "0s"}}}, defaultImagePinsInterval},
		{"an interval under the floor", &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true, Interval: "10s"}}}, minImagePinsInterval},
		{"a configured interval", &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true, Interval: "2h"}}}, 2 * time.Hour},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := imagePinsInterval(tt.config); got != tt.want {
				t.Errorf("imagePinsInterval = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStartImagePinsTicker: the patrol gets its own ticker when it is on, at the
// interval its config entry names, and none when the town does not name it
// (gt-01lcq2).
func TestStartImagePinsTicker(t *testing.T) {
	t.Parallel()
	var intervals []time.Duration
	ticks := make(chan time.Time)
	stops := 0
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
	d.newImagePinsTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}

	if channel, stop := d.startImagePinsTicker(); channel != nil || stop != nil {
		t.Error("a town that does not name image_pins started its ticker")
	}
	if len(intervals) != 0 {
		t.Errorf("a town that does not name image_pins asked for tickers at %v, want none", intervals)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true}}}
	channel, stop := d.startImagePinsTicker()
	if channel == nil || stop == nil {
		t.Fatal("a town that names image_pins started no ticker")
	}
	if len(intervals) != 1 || intervals[0] != defaultImagePinsInterval {
		t.Errorf("the ticker asks for an interval of %v, want the %v default", intervals, defaultImagePinsInterval)
	}
	stop()
	if stops != 1 {
		t.Errorf("the stop func ran %d times, want the ticker stopped once", stops)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true, Interval: "90m"}}}
	channel, stop = d.startImagePinsTicker()
	if channel == nil || stop == nil {
		t.Fatal("a town that names image_pins with an interval started no ticker")
	}
	if len(intervals) != 2 || intervals[1] != 90*time.Minute {
		t.Errorf("the ticker asks for an interval of %v, want the configured 90m", intervals)
	}
	stop()

	// The production ticker, with no seam: the patrol still gets a channel and
	// a stop func, so Run's defer has a real ticker to stop.
	plain := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true}}},
	}
	channel, stop = plain.startImagePinsTicker()
	if channel == nil || stop == nil {
		t.Fatal("the patrol started no ticker without a ticker seam")
	}
	stop()
}

// imagePinsTestDaemon is a daemon whose image_pins tick reads the test's seams.
func imagePinsTestDaemon(t *testing.T, logged io.Writer) *Daemon {
	t.Helper()
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(logged, "", 0),
		clock:        clockwork.NewFakeClockAt(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: true}}},
	}
}

// TestTriggerImagePinsTicksThroughTheTownSeams is the patrol's wiring test
// (gt-01lcq2): one trigger runs the tick over the seams built for the town, and
// the bead the planner asks for is filed and named in the log.
func TestTriggerImagePinsTicksThroughTheTownSeams(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := imagePinsTestDaemon(t, &logged)
	counts := &tickCounts{}
	store := &refreshStore{}
	pins := []ImagePinState{refreshPin("bd", pinBD, tipBD, 20)}
	now := d.clk().Now()
	var built int
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		built++
		return tickSeams(counts, now, pins, nil, store)
	}

	d.triggerImagePins()

	if built != 1 {
		t.Fatalf("the trigger built the town's seams %d times, want 1", built)
	}
	if counts.pins != 1 || counts.tips != 1 || counts.open != 1 || counts.file != 1 {
		t.Errorf("seam calls (pins, tips, open, file) = (%d, %d, %d, %d), want (1, 1, 1, 1)",
			counts.pins, counts.tips, counts.open, counts.file)
	}
	if len(store.created) != 1 {
		t.Fatalf("beads filed = %d, want the one the planner asked for", len(store.created))
	}
	if !strings.Contains(logged.String(), "filed the refresh bead "+store.created[0].ID) {
		t.Errorf("the log does not name the filed bead:\n%s", logged.String())
	}
}

// TestTriggerImagePinsOffBuildsNoSeams: a switched-off patrol builds no seam and
// reads nothing, whether its key is off or the daemon holds no config at all.
func TestTriggerImagePinsOffBuildsNoSeams(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := imagePinsTestDaemon(t, &logged)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: false}}}
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		t.Error("a disabled patrol built the town's seams")
		return imagePinsTickSeams{}
	}
	d.triggerImagePins()

	noConfig := &Daemon{config: nil, logger: log.New(io.Discard, "", 0)}
	noConfig.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		t.Error("a daemon with no config built the town's seams")
		return imagePinsTickSeams{}
	}
	noConfig.triggerImagePins()
}

// TestTriggerImagePinsLogsAFailedTick: a tick that cannot read the pins ends in
// a log line naming the failure, never the silence of a town with nothing due
// (gt-01lcq2).
func TestTriggerImagePinsLogsAFailedTick(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := imagePinsTestDaemon(t, &logged)
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		seams := tickSeams(&tickCounts{}, d.clk().Now(), nil, nil, &refreshStore{})
		seams.readPins = func() ([]byte, error) { return nil, errors.New("the pins file is not there") }
		return seams
	}

	d.triggerImagePins()

	got := logged.String()
	if !strings.Contains(got, "image_pins:") || !strings.Contains(got, "the pins file is not there") {
		t.Errorf("the failed tick is not logged with its failure:\n%s", got)
	}
	if strings.Contains(got, "filed the refresh bead") {
		t.Errorf("a failed tick logged a filed bead:\n%s", got)
	}
}

// TestImagePinsSkipsATickWhileOneRuns: a tick that arrives while the previous
// one still reads is skipped and logged, and the guard is released when that
// tick ends so the next one runs (gt-01lcq2).
func TestImagePinsSkipsATickWhileOneRuns(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := imagePinsTestDaemon(t, &logged)
	d.ctx = context.Background()
	reads := make(chan struct{}, 1)
	release := make(chan struct{})
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		seams := tickSeams(&tickCounts{}, d.clk().Now(), []ImagePinState{refreshPin("bd", pinBD, tipBD, 0)}, nil, &refreshStore{})
		seams.readPins = func() ([]byte, error) {
			reads <- struct{}{}
			<-release
			return tickPinsFile, nil
		}
		return seams
	}

	d.startImagePinsTick()
	waitForSignal(t, reads, "the first tick never reached the pins read")
	d.startImagePinsTick()
	if !strings.Contains(logged.String(), "previous tick still running, skipping") {
		t.Errorf("the second tick was not logged as skipped:\n%s", logged.String())
	}
	close(release)
	d.imagePinsCycles.Wait()
	if d.imagePinsRunning.Load() {
		t.Error("the guard stayed set after the tick ended, so no later tick could run")
	}
}

// TestReadImagePinsFile: the pins file is read from the gt checkout the ensure
// seam names, and a checkout that cannot be resolved or a file that is not there
// is the read's error, never an empty file (gt-01lcq2).
func TestReadImagePinsFile(t *testing.T) {
	t.Parallel()
	checkout := t.TempDir()
	path := filepath.Join(checkout, filepath.FromSlash(daemonimage.PinsPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tickPinsFile, 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	d.imagePinsEnsureFn = func(context.Context, string, string, string) (string, error) {
		return checkout, nil
	}
	got, err := d.readImagePinsFile(context.Background())
	if err != nil {
		t.Fatalf("readImagePinsFile: %v", err)
	}
	if !slices.Equal(got, tickPinsFile) {
		t.Errorf("readImagePinsFile = %q, want %q", got, tickPinsFile)
	}

	d.imagePinsEnsureFn = func(context.Context, string, string, string) (string, error) {
		return "", errors.New("rig gastown has no bare repo")
	}
	if got, err := d.readImagePinsFile(context.Background()); err == nil || !strings.Contains(err.Error(), "rig gastown has no bare repo") {
		t.Errorf("an unresolvable checkout = (%q, %v), want the ensure failure", got, err)
	}

	d.imagePinsEnsureFn = func(context.Context, string, string, string) (string, error) {
		return t.TempDir(), nil
	}
	if _, err := d.readImagePinsFile(context.Background()); err == nil || !strings.Contains(err.Error(), daemonimage.PinsPath) {
		t.Errorf("a missing pins file = %v, want an error naming %s", err, daemonimage.PinsPath)
	}
}

// TestImagePinsCompareRefusesATownWithNoForgejo: a tick that cannot build the
// compare reports it instead of a read that can count no pin's lag (gt-01lcq2).
func TestImagePinsCompareRefusesATownWithNoForgejo(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	cmp, err := d.imagePinsCompare()
	if err == nil {
		t.Fatal("a town whose rigs name no Forgejo built a compare")
	}
	if cmp != nil {
		t.Errorf("imagePinsCompare = %v with an error, want no compare", cmp)
	}
	if !strings.Contains(err.Error(), "image pins patrol") {
		t.Errorf("error %q does not name the patrol", err)
	}
}
