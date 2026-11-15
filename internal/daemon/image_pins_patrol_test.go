package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
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
