package dashboard

import (
	"encoding/json"
	"testing"
	"time"
)

// The Landings pane is rebuilt only when something it draws from changed
// (gt-urccnx): a working seat's times move on every frame, but the pane draws
// only the seats' names and rigs, so a frame that moves only those times leaves
// the 24-hour table where it is.

// trendState is one page state with a day's landing row; the clock is fixed, so
// two states differ only where a test changes them.
func trendState(polecats []any, followups map[string]any) map[string]any {
	now := time.Date(2026, 11, 26, 17, 20, 0, 0, time.Local)
	s := map[string]any{"trend": map[string]any{
		"hours":  []any{map[string]any{"hour": now.Truncate(time.Hour).Format(time.RFC3339), "landed": 1, "rejected": 0}},
		"stages": []any{},
		"recent": []any{map[string]any{"at": now.Add(-5 * time.Minute).Format(time.RFC3339), "bead": "gt-landed", "rig": "gastown", "outcome": "landed"}},
	}}
	if polecats != nil {
		s["polecats"] = polecats
	}
	if followups != nil {
		s["followups"] = followups
	}
	return s
}

// trendSeat is one working seat whose times a frame moves on its own.
func trendSeat(rig, name string, working, elapsed, quiet int) map[string]any {
	p := seat(rig, name, "working", "gt-vcr3z", "one Work panel", "deepseek-flash", 0)
	p["working_sec"], p["elapsed_sec"], p["quiet_sec"] = working, elapsed, quiet
	return p
}

// drawnTrendRows remembers the landing rows #trend holds and answers how many
// there are, then a function answering how many rows it holds now and how many
// of those are new elements: a redraw builds new rows, so identity says whether
// the pane was left alone.
func drawnTrendRows(t *testing.T, dom *pageDOM) (int, func() (now, replaced int)) {
	t.Helper()
	const walk = `var out = []; __walk(__byId["trend"], function (c) { if (__cls(c, "ltrow")) out.push(c); });`
	n := pageNumber(t, dom, `(function () { `+walk+` window.__trendRows = out; return out.length; })()`)
	return n, func() (int, int) {
		raw := pageExpr(t, dom, `(function () { `+walk+` var before = window.__trendRows || [], kept = 0;
			for (var i = 0; i < out.length; i++) if (before.indexOf(out[i]) >= 0) kept++;
			window.__trendRows = out;
			return JSON.stringify([out.length, out.length - kept]); })()`)
		var got []int
		if err := json.Unmarshal([]byte(raw), &got); err != nil || len(got) != 2 {
			t.Fatalf("reading the Landings row comparison from %q: %v", raw, err)
		}
		return got[0], got[1]
	}
}

func TestTheLandingsPaneRedrawsOnlyWhenWhatItDrawsMoves(t *testing.T) {
	t.Parallel()
	seatState := func(rig, name string, times ...int) map[string]any {
		return trendState([]any{trendSeat(rig, name, times[0], times[1], times[2])}, nil)
	}
	followups := func(open int) map[string]any {
		return map[string]any{"open": open, "filed_week": 1, "share_pct": 4}
	}
	tests := []struct {
		name          string
		before, after map[string]any
		redraw        bool
	}{
		{"only the seats' times move", seatState("gastown", "dementus", 60, 120, 5), seatState("gastown", "dementus", 61, 121, 6), false},
		{"an unnamed seat's times move",
			seatState("gastown", "dementus", 60, 120, 5),
			trendState([]any{trendSeat("gastown", "dementus", 60, 120, 5), trendSeat("gastown", "", 1, 2, 3)}, nil), false},
		{"a seat is renamed", seatState("gastown", "dementus", 60, 120, 5), seatState("gastown", "furiosa", 60, 120, 5), true},
		{"a seat's rig moves", seatState("gastown", "dementus", 60, 120, 5), seatState("longeye", "dementus", 60, 120, 5), true},
		{"the seats are read for the first time", trendState(nil, nil), trendState([]any{}, nil), true},
		{"the first seat arrives", trendState([]any{}, nil), seatState("gastown", "dementus", 60, 120, 5), true},
		{"only followups move",
			trendState([]any{trendSeat("gastown", "dementus", 60, 120, 5)}, followups(3)),
			trendState([]any{trendSeat("gastown", "dementus", 60, 120, 5)}, followups(4)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dom := newPageDOM(t)
			pushState(t, dom, tt.before)
			n, compare := drawnTrendRows(t, dom)
			if n != 2 {
				t.Fatalf("the first push drew %d rows, want a header and the landed row", n)
			}
			pushState(t, dom, tt.after)
			now, replaced := compare()
			if now != n {
				t.Errorf("the second push left %d rows, want the %d a drawn table has", now, n)
			}
			if (replaced != 0) != tt.redraw {
				t.Errorf("the second push replaced %d rows, redraw want %v", replaced, tt.redraw)
			}
		})
	}
}
