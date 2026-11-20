package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
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
		file: func(pins []ImagePinState, filedAt time.Time) (string, error) {
			counts.file++
			return imagePinsFileRefreshBead(store.Create, pins, filedAt)
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
			pins: []ImagePinState{refreshPin("bd", pinBD, tipBD, 14)},
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
				// The filed bead says when its tips were read: the tick hands the
				// filer the same clock read it gave the planner.
				if want := "These tips were read at " + now.UTC().Format(time.RFC3339); !strings.Contains(store.created[0].Description, want) {
					t.Errorf("filed description does not name the patrol's clock %q:\n%s", want, store.created[0].Description)
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
		file: func([]ImagePinState, time.Time) (string, error) {
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
				file: func([]ImagePinState, time.Time) (string, error) {
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

// imagePinsCollected returns what the attention tick's collector for kind
// raises from d, as a town tick would see it.
func imagePinsCollected(t *testing.T, d *Daemon, kind attention.Kind) []attention.Item {
	t.Helper()
	c := collectorFor(t, d.attentionSources(d.clk().Now()), kind)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("%s collector: %v", kind, err)
	}
	return items
}

// TestImagePinsFailedTickRaisesUnreadable: a tick that cannot read the pins is
// image-pins-unreadable in the attention queue, never silence, and the next
// tick that reads cleanly clears it (gt-01lcq2).
func TestImagePinsFailedTickRaisesUnreadable(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := imagePinsTestDaemon(t, &logged)
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		seams := tickSeams(&tickCounts{}, d.clk().Now(), nil, nil, &refreshStore{})
		seams.readPins = func() ([]byte, error) { return nil, errors.New("the pins file is not there") }
		return seams
	}

	d.triggerImagePins()

	items := imagePinsCollected(t, d, attention.KindImagePinsUnreadable)
	if len(items) != 1 {
		t.Fatalf("a failed tick raised %d unreadable item(s), want 1: %+v", len(items), items)
	}
	if items[0].Key != imagePinsUnreadableKey {
		t.Errorf("item key = %q, want %q", items[0].Key, imagePinsUnreadableKey)
	}
	if !strings.Contains(items[0].Summary, "the pins file is not there") {
		t.Errorf("item summary %q does not name the failure", items[0].Summary)
	}

	// The next tick reads a pin at its tip: nothing is unreadable any more, so
	// the item clears rather than outliving the failure.
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		return tickSeams(&tickCounts{}, d.clk().Now(), []ImagePinState{refreshPin("bd", pinBD, tipBD, 0)}, nil, &refreshStore{})
	}
	d.triggerImagePins()

	if items := imagePinsCollected(t, d, attention.KindImagePinsUnreadable); len(items) != 0 {
		t.Errorf("a clean tick left %+v in the queue, want the failure cleared", items)
	}
}

// TestImagePinsItemsClearOnTheNextTickThatFindsThemGone is the acceptance test
// for gt-9oap8n: the tick that finds a pin stale, a pin unreadable and the
// refresh bead stuck raises each item, and the next tick that finds none of
// those conditions raises nothing, so the queue clears an item rather than it
// outliving its cause. The whole tick's own failure is
// TestImagePinsFailedTickRaisesUnreadable's.
func TestImagePinsItemsClearOnTheNextTickThatFindsThemGone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kind  attention.Kind
		key   string
		first func(*Daemon) imagePinsTickSeams
	}{
		{
			name: "a pin at the stale bound",
			kind: attention.KindImagePinsStale,
			key:  imagePinsStaleKey + ":bd",
			first: func(d *Daemon) imagePinsTickSeams {
				return tickSeams(&tickCounts{}, d.clk().Now(),
					[]ImagePinState{refreshPin("bd", pinBD, tipBD, imagePinsStaleBehind)}, nil, &refreshStore{})
			},
		},
		{
			// Behind is set with the error so a tick that trusted a failed
			// read's distance would raise stale instead of unreadable.
			name: "a pin whose read failed",
			kind: attention.KindImagePinsUnreadable,
			key:  imagePinsUnreadableKey + ":bd",
			first: func(d *Daemon) imagePinsTickSeams {
				failed := ImagePinState{Name: "bd", Pin: pinBD, Tip: tipBD, Behind: imagePinsStaleBehind, Err: errors.New("ls-remote exited 128")}
				return tickSeams(&tickCounts{}, d.clk().Now(),
					[]ImagePinState{failed}, nil, &refreshStore{})
			},
		},
		{
			name: "a refresh bead stuck unlanded",
			kind: attention.KindImagePinsRefreshStuck,
			key:  imagePinsRefreshStuckKey,
			first: func(d *Daemon) imagePinsTickSeams {
				open := &ImagePinRefreshBead{ID: "gt-refresh4", OpenedAt: d.clk().Now().Add(-imagePinsRefreshStuck - time.Hour)}
				return tickSeams(&tickCounts{}, d.clk().Now(),
					[]ImagePinState{refreshPin("bd", pinBD, tipBD, 0)}, open, &refreshStore{})
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logged bytes.Buffer
			d := imagePinsTestDaemon(t, &logged)
			d.newImagePinsSeams = func(context.Context) imagePinsTickSeams { return tt.first(d) }

			d.triggerImagePins()

			raised := imagePinsCollected(t, d, tt.kind)
			if len(raised) != 1 || raised[0].Key != tt.key {
				t.Fatalf("the tick that found %s raised %+v, want the one item keyed %q", tt.name, raised, tt.key)
			}

			// The next tick reads the pin at its source tip with no refresh
			// bead open: both conditions are gone, so their items go with them.
			d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
				return tickSeams(&tickCounts{}, d.clk().Now(),
					[]ImagePinState{refreshPin("bd", pinBD, tipBD, 0)}, nil, &refreshStore{})
			}
			d.triggerImagePins()

			if items := imagePinsCollected(t, d, tt.kind); len(items) != 0 {
				t.Errorf("the tick that found the condition gone left %+v, want %s cleared", items, tt.kind)
			}
		})
	}
}

// TestImagePinsPlannerItemsReachTheirCollectors: each planner item reaches the
// collector for its own kind, keyed by its pin, and raises nothing under
// another kind (gt-01lcq2).
func TestImagePinsPlannerItemsReachTheirCollectors(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := imagePinsTestDaemon(t, &logged)
	now := d.clk().Now()
	open := &ImagePinRefreshBead{ID: "gt-refresh4", OpenedAt: now.Add(-4 * time.Hour)}
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		return tickSeams(&tickCounts{}, now, []ImagePinState{refreshPin("bd", pinBD, tipBD, 30)}, open, &refreshStore{})
	}

	d.triggerImagePins()

	stale := imagePinsCollected(t, d, attention.KindImagePinsStale)
	if len(stale) != 1 {
		t.Fatalf("stale items = %+v, want the one pin item", stale)
	}
	if stale[0].Key != imagePinsStaleKey+":bd" {
		t.Errorf("stale item key = %q, want %q", stale[0].Key, imagePinsStaleKey+":bd")
	}
	if stale[0].Severity != attention.SeverityMedium {
		t.Errorf("stale item severity = %q, want the planner's %q", stale[0].Severity, attention.SeverityMedium)
	}
	if !strings.Contains(stale[0].Summary, "30 commits behind") {
		t.Errorf("stale item summary %q does not name the distance", stale[0].Summary)
	}
	stuck := imagePinsCollected(t, d, attention.KindImagePinsRefreshStuck)
	if len(stuck) != 1 || stuck[0].Key != imagePinsRefreshStuckKey {
		t.Errorf("refresh-stuck items = %+v, want the one open-bead item keyed %q", stuck, imagePinsRefreshStuckKey)
	}
	if items := imagePinsCollected(t, d, attention.KindImagePinsUnreadable); len(items) != 0 {
		t.Errorf("a tick that read every pin raised unreadable items %+v", items)
	}
}

// TestImagePinsCollectorIsEmptyWhileThePatrolIsOff: a patrol the town switches
// off has no read to publish, so the items it raised clear rather than outlive
// the setting (gt-01lcq2).
func TestImagePinsCollectorIsEmptyWhileThePatrolIsOff(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := imagePinsTestDaemon(t, &logged)
	d.newImagePinsSeams = func(context.Context) imagePinsTickSeams {
		seams := tickSeams(&tickCounts{}, d.clk().Now(), nil, nil, &refreshStore{})
		seams.readPins = func() ([]byte, error) { return nil, errors.New("the pins file is not there") }
		return seams
	}
	d.triggerImagePins()
	if items := imagePinsCollected(t, d, attention.KindImagePinsUnreadable); len(items) != 1 {
		t.Fatalf("the failed tick raised %+v, want the one unreadable item", items)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{ImagePins: &PatrolConfig{Enabled: false}}}
	if items := imagePinsCollected(t, d, attention.KindImagePinsUnreadable); len(items) != 0 {
		t.Errorf("a switched-off patrol kept %+v in the queue, want none", items)
	}
}

// TestImagePinsAttentionRefusesAnUnknownItemKey: a planner key the collectors
// cannot name is an error the collector returns, never an item dropped
// (gt-01lcq2).
func TestImagePinsAttentionRefusesAnUnknownItemKey(t *testing.T) {
	t.Parallel()
	var a imagePinsAttention
	a.record([]ImagePinItem{{Key: "image-pins-from-a-newer-daemon", Pin: "bd"}}, nil)

	items, err := a.observed()
	if err == nil || !strings.Contains(err.Error(), "image-pins-from-a-newer-daemon") {
		t.Fatalf("observed = (%+v, %v), want an error naming the unknown key", items, err)
	}
	s := &attentionSources{imagePins: a.observed}
	if _, err := s.collectImagePins(attention.KindImagePinsStale)(context.Background()); err == nil {
		t.Error("the collector dropped the unknown item instead of returning the error")
	}
}

// TestStopImagePinsPatrolJoinsTheRunningTick: Run's return stops the ticker and
// waits for the tick it started, and a tick still running at the bound is
// logged and left (gt-01lcq2).
func TestStopImagePinsPatrolJoinsTheRunningTick(t *testing.T) {
	t.Parallel()
	t.Run("the tick the ticker started is joined", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := imagePinsTestDaemon(t, &logged)
		d.ctx = context.Background()
		d.imagePinsJoinWait = time.Hour
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
		waitForSignal(t, reads, "the tick never reached the pins read")

		stopped := make(chan struct{})
		stopCalled := make(chan struct{})
		go func() {
			d.stopImagePinsPatrol(func() { close(stopCalled) })
			close(stopped)
		}()
		waitForSignal(t, stopCalled, "the stop func never ran")
		select {
		case <-stopped:
			t.Fatal("stopImagePinsPatrol returned while the tick still ran")
		default:
		}
		close(release)
		waitForSignal(t, stopped, "stopImagePinsPatrol never returned after the tick finished")
		if d.imagePinsRunning.Load() {
			t.Error("the join returned with the tick's guard still set")
		}
	})

	t.Run("a tick past the bound is logged and left", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := imagePinsTestDaemon(t, &logged)
		d.ctx = context.Background()
		d.imagePinsJoinWait = 30 * time.Millisecond
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
		waitForSignal(t, reads, "the tick never reached the pins read")

		d.stopImagePinsPatrol(nil)
		if !strings.Contains(logged.String(), "still running") {
			t.Errorf("the left-behind tick is not logged:\n%s", logged.String())
		}

		close(release)
		d.imagePinsCycles.Wait()
	})
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
// Each case asserts the cause it names: a town with a readable registry and no
// rig is the empty rig list, and a rig with no remote is the URL the compare
// cannot be built from.
func TestImagePinsCompareRefusesATownWithNoForgejo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		townJSON string
		want     string
	}{
		{
			name:     "a town whose rig list is empty",
			townJSON: `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","registry":{"rigs":{}}}`,
			want:     "rig list is unreadable or empty",
		},
		{
			name:     "a town whose one rig names no remote",
			townJSON: `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","registry":{"rigs":{"demo":{}}}}`,
			want:     "not a URL with a scheme and host",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(tc.townJSON), 0o644); err != nil {
				t.Fatal(err)
			}
			d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
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
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name its cause %q", err, tc.want)
			}
		})
	}
}

// TestReadImagePinTipsRefusesWithoutACompare: a tip read that cannot build the
// compare reports it, instead of reading the file back at zero lag (gt-01lcq2).
func TestReadImagePinTipsRefusesWithoutACompare(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	pins, err := d.readImagePinTips(context.Background(), tickPinsFile)
	if err == nil {
		t.Fatalf("readImagePinTips = %+v, want the compare's refusal", pins)
	}
	if pins != nil {
		t.Errorf("a refused read returned states: %+v", pins)
	}
}

// TestImagePinsTipGit: the tip read runs the ls-remote of the source's main,
// answers git's stdout on success, and reports a failed call by the call and its
// bound, never by git's output, which a failed fetch can fill with the URL it was
// given (gt-01lcq2).
func TestImagePinsTipGit(t *testing.T) {
	t.Parallel()
	const url = "https://forge.example.com/town/bd"
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		answer  []byte
		runErr  error
		ctx     func() (context.Context, context.CancelFunc)
		want    string
		wantSub string
	}{
		{
			name:   "git answers the ref",
			answer: []byte(tipBD + "\t" + pinTipRef + "\n"),
			want:   tipBD + "\t" + pinTipRef + "\n",
			ctx:    func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
		},
		{
			name:    "git fails",
			runErr:  errors.New("exit status 128"),
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantSub: "git ls-remote " + pinTipRef + ": exit status 128",
		},
		{
			name:   "the call's bound has passed",
			runErr: errors.New("signal: killed"),
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), now.Add(-time.Second))
			},
			wantSub: "did not answer within " + imagePinsGitTimeout.String(),
		},
		{
			name:   "the tick was canceled",
			runErr: errors.New("signal: killed"),
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantSub: "stopped before it answered",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := tt.ctx()
			defer cancel()
			var argv []string
			d := &Daemon{logger: log.New(io.Discard, "", 0)}
			d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
				argv = cmd.Args
				return tt.answer, []byte("fatal: could not read"), tt.runErr
			}

			got, err := d.imagePinsTipGit(ctx)(url)

			want := []string{"git", "ls-remote", "--refs", url, pinTipRef}
			if !slices.Equal(argv, want) {
				t.Errorf("argv = %v, want %v", argv, want)
			}
			if tt.wantSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("error = %v, want it to name %q", err, tt.wantSub)
				}
				if strings.Contains(err.Error(), "fatal: could not read") {
					t.Errorf("error %q carries git's output", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the tip read = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("the tip read = %q, want git's answer %q", got, tt.want)
			}
		})
	}
}

// imagePinsPathCountResult runs the production path count over the fixture's
// devops pin, with run answering git, and returns its answer and error.
func imagePinsPathCountResult(t *testing.T, run cmdRunFunc) (int, error) {
	t.Helper()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	d.execCmd = run
	return d.imagePinsPathCount(context.Background())(pinsReadPinURL("devops"), pinsReadDevopsCommit, pinsReadDevopsTip, []string{"cmd/"})
}

// TestImagePinsPathCountRunsFilteredFetchThenRevList: the production path count
// fetches the source's main, filtered to no blobs, into the scratch ref of the
// devops rig clone, then counts the commits from the pin to the tip that touch
// the paths (gt-wz5ct4).
func TestImagePinsPathCountRunsFilteredFetchThenRevList(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := filepath.Join(town, "devops", ".repo.git")
	url, pin, tip := pinsReadPinURL("devops"), pinsReadDevopsCommit, pinsReadDevopsTip
	paths := []string{"cmd/", "deploy/"}
	var argv [][]string
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		argv = append(argv, cmd.Args)
		return []byte("1\n"), nil, nil
	}

	got, err := d.imagePinsPathCount(context.Background())(url, pin, tip, paths)
	if err != nil || got != 1 {
		t.Fatalf("the path count = %d, %v; want git's answer 1", got, err)
	}
	if len(argv) != 2 {
		t.Fatalf("git ran %d time(s), want the fetch and then the count: %v", len(argv), argv)
	}
	wantFetch := []string{"git", "--git-dir=" + repo, "fetch", "--filter=blob:none", url, "+main:refs/gt/pin-tip"}
	if !slices.Equal(argv[0], wantFetch) {
		t.Errorf("the first git call = %v, want the filtered fetch %v", argv[0], wantFetch)
	}
	wantCount := append([]string{"git", "--git-dir=" + repo, "rev-list", "--count", pin + ".." + tip, "--"}, paths...)
	if !slices.Equal(argv[1], wantCount) {
		t.Errorf("the second git call = %v, want the path-limited count %v", argv[1], wantCount)
	}
}

// TestImagePinsPathCountFetchFailureIsAnError: a fetch or a count that fails is
// an error naming the call and carrying git's first stderr line, and a failed
// fetch is not followed by a count (gt-wz5ct4).
func TestImagePinsPathCountFetchFailureIsAnError(t *testing.T) {
	t.Parallel()
	const fatal = "fatal: could not read username for 'http://forgejo:3000'"
	for _, tt := range []struct {
		name, fails string
		stderr      []byte
		want        string
	}{
		{"the fetch fails", "fetch", []byte(fatal + "\nhint: more\n"), "git fetch: git exited 128: " + fatal},
		{"the rev-list fails", "rev-list", []byte(fatal + "\nhint: more\n"), "git rev-list: git exited 128: " + fatal},
		{"the fetch fails silently", "fetch", nil, "git fetch: git exited 128"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			boom := errors.New("git exited 128")
			calls := 0
			n, err := imagePinsPathCountResult(t, func(cmd *exec.Cmd) ([]byte, []byte, error) {
				calls++
				if slices.Contains(cmd.Args, tt.fails) {
					return []byte("stdout that is not the error"), tt.stderr, boom
				}
				return nil, nil, nil
			})
			if !errors.Is(err, boom) || n != 0 || err == nil || err.Error() != tt.want {
				t.Fatalf("a failed %s = %d, %v; want %q, with no count", tt.fails, n, err, tt.want)
			}
			if tt.fails == "fetch" && calls != 1 {
				t.Errorf("git ran %d times, want the failed fetch alone", calls)
			}
		})
	}
}

// TestImagePinsPathCountNonNumberIsAnError: a count git answers with anything but
// a count of commits, a non-number or a negative one, is an error, never a zero
// that reads as a pin touching none of its inputs (gt-wz5ct4).
func TestImagePinsPathCountNonNumberIsAnError(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, answer, want string }{
		{"a non-number", "lots\n", "non-number"},
		{"a negative count", "-3\n", "negative"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, err := imagePinsPathCountResult(t, func(*exec.Cmd) ([]byte, []byte, error) {
				return []byte(tt.answer), nil, nil
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("%s count = %d, %v; want an error saying %q", tt.name, n, err, tt.want)
			}
		})
	}
}

// TestImagePinsPathCountRefusesANegativeCount: a count git answers below zero is
// an error that names the count, never a count a caller could read as a lag
// (gt-yeiehx).
func TestImagePinsPathCountRefusesANegativeCount(t *testing.T) {
	t.Parallel()
	n, err := imagePinsPathCountResult(t, func(*exec.Cmd) ([]byte, []byte, error) {
		return []byte("-3\n"), nil, nil
	})
	if err == nil || n != 0 || !strings.Contains(err.Error(), "-3") {
		t.Fatalf("a negative count = %d, %v; want an error naming the count -3, with no count", n, err)
	}
}

// imagePinsPathCountCalled runs the production path count over pin and tip and
// returns its answer, its error, and how many git calls it made.
func imagePinsPathCountCalled(t *testing.T, pin, tip string) (int, int, error) {
	t.Helper()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	calls := 0
	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
		calls++
		return []byte("1\n"), nil, nil
	}
	n, err := d.imagePinsPathCount(context.Background())(pinsReadPinURL("devops"), pin, tip, []string{"cmd/"})
	return n, calls, err
}

// imagePinsRefusedShas are the values a pin or tip must refuse before git sees
// them: an option git would parse, a hyphen-led value, a short sha, an upper-case
// sha and an empty value (gt-yeiehx).
var imagePinsRefusedShas = []struct{ name, sha string }{
	{"an option-shaped value", "--upload-pack=x"},
	{"a hyphen-led value", "-4444444444444444444444444444444444444444"},
	{"a short sha", "444444"},
	{"an upper-case sha", "ABCDEF0123456789ABCDEF0123456789ABCDEF01"},
	{"an empty value", ""},
}

// TestImagePinsPathCountRefusesAPinThatIsNotFortyLowercaseHex: a pin that is not
// 40 lowercase hex is an error naming the pin, and no git call runs, the fetch
// included (gt-yeiehx).
func TestImagePinsPathCountRefusesAPinThatIsNotFortyLowercaseHex(t *testing.T) {
	t.Parallel()
	for _, tt := range imagePinsRefusedShas {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, calls, err := imagePinsPathCountCalled(t, tt.sha, pinsReadDevopsTip)
			if err == nil || n != 0 || !strings.Contains(err.Error(), strconv.Quote(tt.sha)) {
				t.Fatalf("a pin of %s = %d, %v; want an error naming the pin %q, with no count", tt.name, n, err, tt.sha)
			}
			if calls != 0 {
				t.Errorf("git ran %d time(s) for a refused pin, want none", calls)
			}
		})
	}
}

// TestImagePinsPathCountRefusesATipThatIsNotFortyLowercaseHex: a tip that is not
// 40 lowercase hex is an error naming the tip, and no git call runs, the fetch
// included (gt-yeiehx).
func TestImagePinsPathCountRefusesATipThatIsNotFortyLowercaseHex(t *testing.T) {
	t.Parallel()
	for _, tt := range imagePinsRefusedShas {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, calls, err := imagePinsPathCountCalled(t, pinsReadDevopsCommit, tt.sha)
			if err == nil || n != 0 || !strings.Contains(err.Error(), strconv.Quote(tt.sha)) {
				t.Fatalf("a tip of %s = %d, %v; want an error naming the tip %q, with no count", tt.name, n, err, tt.sha)
			}
			if calls != 0 {
				t.Errorf("git ran %d time(s) for a refused tip, want none", calls)
			}
		})
	}
}

// TestImagePinsPatrolWiresThePathCountSeam: the patrol's production read passes
// the path count, so a devops pin's lag is the count the seam answers through
// git; a read wired with no seam reports that pin's Err instead (gt-wz5ct4).
func TestImagePinsPatrolWiresThePathCountSeam(t *testing.T) {
	t.Parallel()
	d := forgejoTownDaemon(t, forgejoTown(t, map[string]string{"gastown": rigForgejoSettings("http://forgejo:3000/owner/gastown.git")}))
	token := filepath.Join(t.TempDir(), "forgejo-viewer.env")
	if err := os.WriteFile(token, []byte("FORGEJO_TOKEN=placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.imagePinsTokenFile = token
	// The three compared pins answer at their own commits, so no pin but devops
	// trails its tip and nothing asks the compare for a count.
	tips := map[string]string{
		pinsReadPinURL("beads"):            pinsReadBDCommit,
		pinsReadPinURL("organic-mechanic"): pinsReadOMCommit,
		pinsReadPinURL("history-man"):      pinsReadHMCommit,
		pinsReadPinURL("devops"):           pinsReadDevopsTip,
	}
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		switch {
		case slices.Contains(cmd.Args, "ls-remote"):
			return []byte(pinsReadLS(tips[cmd.Args[len(cmd.Args)-2]])), nil, nil
		case slices.Contains(cmd.Args, "rev-list"):
			return []byte("3\n"), nil, nil
		}
		return nil, nil, nil
	}

	states, err := d.readImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL))
	if err != nil {
		t.Fatalf("readImagePinTips: %v", err)
	}
	want := ImagePinState{Name: "devops", Pin: pinsReadDevopsCommit, Tip: pinsReadDevopsTip, Behind: 3}
	if len(states) != 4 || states[3] != want {
		t.Fatalf("the read = %+v, want its four pins with devops as %+v: the patrol must count it through its path count seam", states, want)
	}

	// A rev-list that fails leaves devops's state with its Err and no count: the
	// Behind it carries is zero, and the Err is git's first stderr line.
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		switch {
		case slices.Contains(cmd.Args, "ls-remote"):
			return []byte(pinsReadLS(tips[cmd.Args[len(cmd.Args)-2]])), nil, nil
		case slices.Contains(cmd.Args, "rev-list"):
			return nil, []byte("fatal: bad revision\nhint: more\n"), errors.New("git exited 128")
		}
		return nil, nil, nil
	}
	states, err = d.readImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL))
	if err != nil {
		t.Fatalf("readImagePinTips with a failed count: %v", err)
	}
	if states[3].Err == nil || states[3].Behind != 0 || !strings.Contains(states[3].Err.Error(), "fatal: bad revision") {
		t.Fatalf("devops with a failed count = %+v, want its Err carrying git's first stderr line and no count", states[3])
	}

	// A read wired with no seam names the missing path count on devops's Err.
	states, err = ReadImagePinTips(context.Background(), pinsReadFixture(pinsReadPinURL), nil, d.imagePinsTipGit(context.Background()), nil, &fakePinCompare{})
	if err != nil {
		t.Fatalf("ReadImagePinTips with no seam: %v", err)
	}
	if states[3].Err == nil || !strings.Contains(states[3].Err.Error(), "no path count") {
		t.Fatalf("devops with no seam = %+v, want its Err to name the missing path count", states[3])
	}
}

// TestImagePinsCompareForRefusesAMissingToken: the compare needs the viewer's
// token, and a token file that is not there is the build's error, never a client
// that reads as anonymous (gt-01lcq2).
func TestImagePinsCompareForRefusesAMissingToken(t *testing.T) {
	t.Parallel()
	cmp, err := imagePinsCompareFor("https://forge.example.com/api/v1", filepath.Join(t.TempDir(), "forgejo-viewer.env"))
	if err == nil {
		t.Fatal("a missing token file built a compare")
	}
	if cmp != nil {
		t.Errorf("imagePinsCompareFor = %v with an error, want no compare", cmp)
	}
	if !strings.Contains(err.Error(), "image pins patrol") {
		t.Errorf("error %q does not name the patrol", err)
	}
}

// bdCallRecord is one bd call a seam was asked to run: the directory bd would
// run in, the environment it would start from and its argv.
type bdCallRecord struct {
	dir  string
	env  []string
	args []string
}

// TestImagePinsBeadsOpensTheGTSourceRigDatabase is the client's test
// (gt-fu9c8a): the patrol panicked the daemon on its first tick because
// imagePinsBeads called WithTimeout on a NewPinned wrapper, so this test calls
// the constructor itself and drives one bd call through the client it returns.
// That call must run in the gt source rig's .beads directory, and a client whose
// copies dropped its bd seam would not answer at all. The env that call carries
// is TestImagePinsBeadsRoutesWithTheRigsBeadsDir's subject.
func TestImagePinsBeadsOpensTheGTSourceRigDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(io.Discard, "", 0)}
	var calls []bdCallRecord
	d.imagePinsBeadRunner = func(_ context.Context, dir string, env, args []string) ([]byte, []byte, error) {
		calls = append(calls, bdCallRecord{dir: dir, env: env, args: args})
		return []byte("[]"), nil, nil
	}

	store := d.imagePinsBeads()
	if _, err := store.List(beads.ListOptions{Label: imagePinsRefreshLabel}); err != nil {
		t.Fatalf("List through the patrol's client: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("bd calls = %d, want the one List", len(calls))
	}
	wantDir := filepath.Join(townRoot, rebuildGTSourceRig, ".beads")
	if calls[0].dir != wantDir {
		t.Errorf("the bd call runs in %s, want the gt source rig's database directory %s", calls[0].dir, wantDir)
	}
}

// TestImagePinsBeadsRoutesWithTheRigsBeadsDir: the client's bd calls start from
// the town's mutation routing environment over the gt source rig's .beads, not
// the process one (gt-ff9cxi). BEADS_DIR names that rig's .beads — the store
// the call reads, which the gateway forwarder places the call in — so a refresh
// bead created with no ID prefix files into the rig's database, not the town's
// (gt-dl9ug0). No database pin reaches bd, so prefix routing chooses the
// database the refresh bead is filed into, and the endpoint the town names is
// the server the call dials.
func TestImagePinsBeadsRoutesWithTheRigsBeadsDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeImagePinsTownDolt(t, townRoot, `,"dolt":{"host":"127.0.0.8","port":4507}`)
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(io.Discard, "", 0)}
	var calls []bdCallRecord
	d.imagePinsBeadRunner = func(_ context.Context, dir string, env, args []string) ([]byte, []byte, error) {
		calls = append(calls, bdCallRecord{dir: dir, env: env, args: args})
		return []byte("[]"), nil, nil
	}

	if _, err := d.imagePinsBeads().List(beads.ListOptions{Label: imagePinsRefreshLabel}); err != nil {
		t.Fatalf("List through the patrol's client: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("bd calls = %d, want the one List", len(calls))
	}

	wantBeadsDir := beads.ResolveBeadsDir(filepath.Join(townRoot, rebuildGTSourceRig, ".beads"))
	got, ok := lookupEnvIn(calls[0].env, "BEADS_DIR")
	if !ok || got != wantBeadsDir {
		t.Errorf("the bd call's BEADS_DIR = %q (set %v), want the gt source rig's .beads %q", got, ok, wantBeadsDir)
	}
	if got == beads.ResolveBeadsDir(townRoot) {
		t.Errorf("the bd call's BEADS_DIR names the town's .beads, so the refresh bead is filed in the town database")
	}
	for _, key := range []string{"BEADS_DB", "BD_DB", "BEADS_DOLT_SERVER_DATABASE"} {
		if value, ok := lookupEnvIn(calls[0].env, key); ok {
			t.Errorf("the bd call carries %s=%s, so the env, not the route table, pins the database", key, value)
		}
	}
	for key, want := range map[string]string{
		"BD_DOLT_AUTO_COMMIT":    "on",
		"BEADS_DOLT_SERVER_HOST": "127.0.0.8",
		"BEADS_DOLT_SERVER_PORT": "4507",
		"BEADS_DOLT_PORT":        "4507",
	} {
		if got, _ := lookupEnvIn(calls[0].env, key); got != want {
			t.Errorf("the bd call's %s = %q, want %q", key, got, want)
		}
	}
}

// writeImagePinsTownDolt writes mayor/town.json under root; extra is appended
// to the town object, so it carries the town's dolt block or nothing.
func writeImagePinsTownDolt(t *testing.T, root, extra string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	town := `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"` + extra + `}`
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(town), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestImagePinsCompareForBuildsTheViewerClient: a readable viewer token builds
// the compare the tick counts a pin's lag with (gt-01lcq2).
func TestImagePinsCompareForBuildsTheViewerClient(t *testing.T) {
	t.Parallel()
	tokenFile := filepath.Join(t.TempDir(), "forgejo-viewer.env")
	if err := os.WriteFile(tokenFile, []byte("FORGEJO_TOKEN=placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmp, err := imagePinsCompareFor("https://forge.example.com/api/v1", tokenFile)
	if err != nil {
		t.Fatalf("imagePinsCompareFor: %v", err)
	}
	if cmp == nil {
		t.Fatal("imagePinsCompareFor built no compare")
	}
}
