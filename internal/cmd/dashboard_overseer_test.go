package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/overseer"
)

// overseerPane builds a reader over fixtures: the heartbeat and journal are
// given, the window is 90 minutes, and the question count is open.
func overseerPane(hb *overseer.Heartbeat, entries []overseer.Entry, open func() (int, error)) *dashOverseerReader {
	return &dashOverseerReader{
		heartbeat: func() (overseer.Heartbeat, bool, error) {
			if hb == nil {
				return overseer.Heartbeat{}, false, nil
			}
			return *hb, true, nil
		},
		journal:       func() ([]overseer.Entry, error) { return entries, nil },
		silentAfter:   func() time.Duration { return 90 * time.Minute },
		openQuestions: open,
	}
}

func TestOverseerPaneIsAliveWhenFreshAndSilentWhenOlderThanTheWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		beat   *time.Time
		want   string
		active *int64
	}{
		{"fresh", ptr(overseerNow.Add(-5 * time.Minute)), dashboard.OverseerAlive, ptr(int64(300))},
		{"at the window", ptr(overseerNow.Add(-90 * time.Minute)), dashboard.OverseerAlive, ptr(int64(5400))},
		{"past the window", ptr(overseerNow.Add(-91 * time.Minute)), dashboard.OverseerSilent, ptr(int64(5460))},
		{"never beat", nil, dashboard.OverseerNone, nil},
	} {
		var hb *overseer.Heartbeat
		if tc.beat != nil {
			hb = &overseer.Heartbeat{TS: *tc.beat, State: overseer.StateIdle, Session: "overseer-1"}
		}
		got := overseerPane(hb, nil, nil).read(overseerNow)
		if got.Status != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got.Status, tc.want)
		}
		switch {
		case tc.active == nil && got.LastActiveSec != nil:
			t.Errorf("%s: last active = %d for a town that never beat", tc.name, *got.LastActiveSec)
		case tc.active != nil && (got.LastActiveSec == nil || *got.LastActiveSec != *tc.active):
			t.Errorf("%s: last active = %v, want %d", tc.name, got.LastActiveSec, *tc.active)
		}
		if got.SilentAfterSec != 5400 {
			t.Errorf("%s: silent window = %d, want 5400", tc.name, got.SilentAfterSec)
		}
		if hb != nil && (got.State != "idle" || got.Session != "overseer-1") {
			t.Errorf("%s: state/session = %q/%q", tc.name, got.State, got.Session)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// The pane counts the last seven days by kind and draws the newest eight lines,
// newest first, from a journal that holds old lines too.
func TestOverseerPaneCountsAWeekAndDrawsTheNewestEightLines(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	entries := []overseer.Entry{
		{TS: overseerNow.Add(-9 * day), Kind: "hold", Text: "old"},
		{TS: overseerNow.Add(-8 * day), Kind: "hold", Text: "old"},
		{TS: overseerNow.Add(-5 * day), Kind: "answer", Text: "a"},
		{TS: overseerNow.Add(-4 * day), Kind: "answer", Text: "b"},
	}
	for i := range 8 {
		entries = append(entries, overseer.Entry{
			TS: overseerNow.Add(-time.Duration(8-i) * time.Hour), Kind: "review", Text: string(rune('A' + i)), Bead: "gt-x",
		})
	}
	got := overseerPane(&overseer.Heartbeat{TS: overseerNow, State: "working"}, entries, nil).read(overseerNow)

	wantCounts := []dashboard.OverseerCount{{Kind: "review", Count: 8}, {Kind: "answer", Count: 2}}
	if len(got.Interventions) != len(wantCounts) {
		t.Fatalf("interventions = %+v, want %+v", got.Interventions, wantCounts)
	}
	for i, w := range wantCounts {
		if got.Interventions[i] != w {
			t.Errorf("interventions[%d] = %+v, want %+v", i, got.Interventions[i], w)
		}
	}
	if len(got.Journal) != dashOverseerLines {
		t.Fatalf("journal has %d lines, want %d", len(got.Journal), dashOverseerLines)
	}
	if first, last := got.Journal[0], got.Journal[7]; first.Text != "H" || first.AgeSec != 3600 || first.Bead != "gt-x" || last.Text != "A" {
		t.Errorf("journal order wrong: first %+v, last %+v", first, last)
	}
}

func TestOverseerPaneQuestionCount(t *testing.T) {
	t.Parallel()
	hb := &overseer.Heartbeat{TS: overseerNow, State: "working"}

	got := overseerPane(hb, nil, func() (int, error) { return 3, nil }).read(overseerNow)
	if got.OpenQuestions == nil || *got.OpenQuestions != 3 {
		t.Errorf("open questions = %v, want 3", got.OpenQuestions)
	}
	// A count that cannot be read is absent, never zero.
	got = overseerPane(hb, nil, func() (int, error) { return 0, errors.New("dolt down") }).read(overseerNow)
	if got.OpenQuestions != nil || got.Unavailable {
		t.Errorf("an unreadable count gave %v, unavailable=%v; want the count absent and the pane up", got.OpenQuestions, got.Unavailable)
	}
}

// A file that cannot be read is an unavailable pane, not a silent overseer.
func TestOverseerPaneIsUnavailableWhenAFileCannotBeRead(t *testing.T) {
	t.Parallel()
	r := overseerPane(nil, nil, nil)
	r.heartbeat = func() (overseer.Heartbeat, bool, error) { return overseer.Heartbeat{}, false, errors.New("parse") }
	if got := r.read(overseerNow); !got.Unavailable || got.Status != "" {
		t.Errorf("corrupt heartbeat read = %+v, want unavailable with no status", got)
	}

	r = overseerPane(&overseer.Heartbeat{TS: overseerNow, State: "working"}, nil, nil)
	r.journal = func() ([]overseer.Entry, error) { return nil, errors.New("permission denied") }
	if got := r.read(overseerNow); !got.Unavailable {
		t.Errorf("unreadable journal read = %+v, want unavailable", got)
	}
}

// The ages never go negative: a heartbeat stamped ahead of the reader's clock
// reads as just now.
func TestOverseerAgeNeverNegative(t *testing.T) {
	t.Parallel()
	if got := overseerAgeSec(overseerNow, overseerNow.Add(time.Hour)); got != 0 {
		t.Errorf("age of a future stamp = %d, want 0", got)
	}
}
