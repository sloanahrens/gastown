package dashboard

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The pane leads with a banner while a gt update roll runs (gt-o8m8ho): its
// step, elapsed and the builds it moves between; an ended roll stays ten minutes.
// A tier sweep running now draws a block too (gt-iw2p6u): a header naming the
// rig, the commit and the cycle's age, then one line per tier of the cycle, with
// no ended banner left behind. An ended roll's pill names the outcome its row
// records, and its time is that roll's own (gt-avxdbf).

func bannerRoll() map[string]any {
	return map[string]any{"run_id": "run-7", "step": "install", "elapsed_sec": 480, "from": "1111aaaa2222", "to": "3333bbbb4444"}
}

// bannerSweep is a running tier sweep as the feed's Now names it.
func bannerSweep() map[string]any {
	return map[string]any{"rig": "gastown", "tier": "race", "elapsed_sec": 300}
}

// bannerNow is the Now block a reading carries: the roll and the sweep, either
// of which is nil when it is not running.
type bannerNow struct {
	roll  map[string]any
	sweep map[string]any
}

// bannerState is one reading of the section: rows, and Now's roll and sweep
// unless nil.
func bannerState(now bannerNow, rows ...any) map[string]any {
	st := systemUpdatesState(systemUpdatesSources{}, rows...)
	n := map[string]any{}
	if now.roll != nil {
		n["roll"] = now.roll
	}
	if now.sweep != nil {
		n["sweep"] = now.sweep
	}
	if len(n) > 0 {
		st["system_updates"].(map[string]any)["now"] = n
	}
	return st
}

// paneBanner is the banner's leading word, or "" when the first line is no banner.
func paneBanner(t *testing.T, dom *pageDOM) string {
	t.Helper()
	if !strings.Contains(dom.descend(t, "systemupdates", 0).Class, "subanner") {
		return ""
	}
	return dom.descend(t, "systemupdates", 0, 0).Text
}

// drawState sets the page's reading and draws it, the way an SSE state frame does.
func drawState(t *testing.T, dom *pageDOM, st map[string]any) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	if _, err := dom.vm.RunString("state = " + string(raw) + "; renderAll();"); err != nil {
		t.Fatalf("drawing the reading: %v", err)
	}
}

// A running roll draws the banner above the rows: rolling, the step, elapsed and
// the short shas from and to.
func TestSystemUpdatesBannerShowsTheRunningRoll(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{roll: bannerRoll()}, systemUpdateGoodRow()))
	if got := dom.element(t, "systemupdates").Kids; got != 2 {
		t.Fatalf("the pane drew %d lines, want the banner and the one row", got)
	}
	for i, want := range []string{"rolling", "install", "8m", "1111aaaa2222 → 3333bbbb4444"} {
		if got := dom.descend(t, "systemupdates", 0, i).Text; got != want {
			t.Errorf("banner cell %d reads %q, want %q", i, got, want)
		}
	}
	if got := dom.descend(t, "systemupdates", 0, 1).Class; got != "tag warnc" {
		t.Errorf("the step chip wears %q, want the running amber", got)
	}
}

// A roll the feed gives no from build reads a dash, never a blank or "undefined".
func TestSystemUpdatesBannerDrawsNoFromAsADash(t *testing.T) {
	t.Parallel()

	roll := bannerRoll()
	delete(roll, "from")
	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{roll: roll}, systemUpdateGoodRow()))
	if got := dom.descend(t, "systemupdates", 0, 3).Text; got != "– → 3333bbbb4444" {
		t.Errorf("a roll with no from build reads %q, want a dash", got)
	}
}

// bannerSweepUnknownState is a reading that names a running sweep in Now while
// the section says the sweep source could not be read, as a failed poll leaves
// the previous Now in place.
func bannerSweepUnknownState(rows ...any) map[string]any {
	st := systemUpdatesState(systemUpdatesSources{sweepUnknown: true}, rows...)
	st["system_updates"].(map[string]any)["now"] = map[string]any{"sweep": bannerSweep()}
	return st
}

// With a roll and a sweep both running, the roll banner is the pane's first line
// and the sweep banner its second.
func TestSystemUpdatesBannerOrdersRollBeforeSweep(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{roll: bannerRoll(), sweep: bannerSweep()}, systemUpdateGoodRow()))
	if got := dom.element(t, "systemupdates").Kids; got != 3 {
		t.Fatalf("the pane drew %d lines, want the two banners and the one row", got)
	}
	if got := dom.descend(t, "systemupdates", 0, 0).Text; got != "rolling" {
		t.Errorf("the first line reads %q, want the roll banner", got)
	}
	if got := dom.descend(t, "systemupdates", 1, 0).Text; got != "sweeping" {
		t.Errorf("the second line reads %q, want the sweep banner", got)
	}
}

// A section that cannot read the sweep draws no sweep banner, even when an older
// Now block still carries a running sweep.
func TestSystemUpdatesBannerSweepUnknown(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", bannerSweepUnknownState(systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "" {
		t.Fatalf("an unknown sweep drew the banner %q", got)
	}
	if got := dom.element(t, "systemupdates").Kids; got != 1 {
		t.Errorf("the pane drew %d lines, want the one commit row", got)
	}
}

// A running tier sweep with no roll draws the pane's first line as its header:
// sweeping, the rig it sweeps, the commit under sweep (a dash when the feed names
// none) and the cycle's age. The tier appears in its own line, never in the
// header (gt-iw2p6u).
func TestSystemUpdatesBannerShowsTheRunningSweep(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{sweep: bannerSweep()}, systemUpdateGoodRow()))
	if got := dom.element(t, "systemupdates").Kids; got != 2 {
		t.Fatalf("the pane drew %d lines, want the header and the one row", got)
	}
	if got := paneBanner(t, dom); got != "sweeping" {
		t.Fatalf("the first line reads %q, want a sweep header", got)
	}
	for i, want := range []string{"gastown", "@ –", "cycle 5m"} {
		if got := dom.descend(t, "systemupdates", 0, i+1).Text; got != want {
			t.Errorf("header cell %d reads %q, want %q", i+1, got, want)
		}
	}
}

// A sweep the feed no longer names leaves no banner on the next push: only the
// commit row is drawn, never an ended sweep's.
func TestSystemUpdatesBannerDropsTheSweepWhenItEnds(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.press(t, "systemupdates", "click")
	drawState(t, dom, bannerState(bannerNow{sweep: bannerSweep()}, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "sweeping" {
		t.Fatalf("the running sweep drew %q, want the banner", got)
	}
	drawState(t, dom, bannerState(bannerNow{}, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "" {
		t.Errorf("the ended sweep still drew the banner %q", got)
	}
	if got := dom.element(t, "systemupdates").Kids; got != 1 {
		t.Errorf("the pane drew %d lines, want the one commit row", got)
	}
}

// An ended roll's banner is dated by the first push that no longer names it,
// even one that skips the pane's redraw, and stays ten minutes to the
// millisecond: exactly ten minutes is still there, a millisecond later gone.
func TestSystemUpdatesBannerKeepsAnEndedRollTenMinutes(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.press(t, "systemupdates", "click")
	drawState(t, dom, bannerState(bannerNow{}, systemUpdateGoodRow()))                                    // the pane's recorded key
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{roll: bannerRoll()}, systemUpdateGoodRow())) // the running roll
	drawState(t, dom, bannerState(bannerNow{}, systemUpdateGoodRow()))                                    // same key: the push skips, still dating the end
	stay := pageNumber(t, dom, "SYSTEM_UPDATES_BANNER_STAY_MS")
	pageExpr(t, dom, "__clock = "+strconv.Itoa(stay))
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{}, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "roll ended" {
		t.Fatalf("at exactly ten minutes the banner reads %q, want it still there", got)
	}
	pageExpr(t, dom, "__clock = "+strconv.Itoa(stay+1))
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{}, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "" {
		t.Errorf("a millisecond past ten minutes the banner reads %q, want it gone", got)
	}
}

// bannerRolledRow is the row the feed draws for the commit a roll rolled to:
// its outcome and its own start and end, 164 seconds apart on 2026-11-23.
func bannerRolledRow(outcome string) map[string]any {
	return map[string]any{
		"sha":  "3333bbbb4444",
		"roll": map[string]any{"outcome": outcome, "started_at": "2026-11-23T10:00:00Z", "ended_at": "2026-11-23T10:02:44Z"},
	}
}

// drawEndedRoll draws a pane that saw the roll run, then a push that no longer
// names it, as the feed ends a roll; the push's state is the pane's recorded
// key, so it skips the redraw and the end is drawn by the next render.
func drawEndedRoll(t *testing.T, dom *pageDOM, rows ...any) {
	t.Helper()
	dom.press(t, "systemupdates", "click")
	drawState(t, dom, bannerState(bannerNow{}, rows...))                                    // the pane's recorded key
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{roll: bannerRoll()}, rows...)) // the running roll
	drawState(t, dom, bannerState(bannerNow{}, rows...))                                    // same key: the push skips, still dating the end
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{}, rows...))                   // the ended roll
}

// An ended roll's pill names the outcome the row for the commit it rolled to
// records, in the class that outcome's stage chip wears, never the step the
// browser last saw (gt-avxdbf).
func TestSystemUpdatesBannerEndedRollNamesItsOutcome(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ outcome, word, class string }{
		{"ok", "ok", "good"},
		{"partial", "partial", "warnc"},
		{"failed", "failed", "badc"},
		// An outcome the page does not know is no outcome to draw: the pill
		// still reads the end, never the step.
		{"running", "ended", ""},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			drawEndedRoll(t, dom, bannerRolledRow(tc.outcome))
			if got := paneBanner(t, dom); got != "roll ended" {
				t.Fatalf("the first line reads %q, want the ended banner", got)
			}
			pill := dom.descend(t, "systemupdates", 0, 1)
			if got := pill.Text; got != tc.word {
				t.Errorf("the pill reads %q, want %q", got, tc.word)
			}
			wantClass := "tag"
			if tc.class != "" {
				wantClass += " " + tc.class
			}
			if got := pill.Class; got != wantClass {
				t.Errorf("the pill wears %q, want %q", got, wantClass)
			}
			for i := 0; i < 4; i++ {
				if got := dom.descend(t, "systemupdates", 0, i).Text; got == "install" {
					t.Errorf("banner cell %d draws the step name %q, which an ended roll never names", i, got)
				}
			}
		})
	}
}

// An ended roll's time is its own row's started_at to ended_at — 164 seconds
// for the 2026-11-23 roll — not the last push's 480 (gt-avxdbf).
func TestSystemUpdatesBannerEndedRollUsesTheRollsOwnTimes(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawEndedRoll(t, dom, bannerRolledRow("ok"))
	if got := dom.descend(t, "systemupdates", 0, 2).Text; got != "3m" {
		t.Errorf("the ended banner reads %q for the time, want the roll's own 164s (3m), not the last push's 8m", got)
	}
}

// A matched row whose own end is not a real time keeps its outcome and the last
// push's elapsed time, never a NaN (gt-avxdbf).
func TestSystemUpdatesBannerEndedRollWithoutItsOwnEndKeepsTheLastPushTime(t *testing.T) {
	t.Parallel()

	row := map[string]any{"sha": "3333bbbb4444", "roll": map[string]any{"outcome": "ok", "started_at": "2026-11-23T10:00:00Z"}}
	dom := newPageDOM(t)
	drawEndedRoll(t, dom, row)
	if got := dom.descend(t, "systemupdates", 0, 1).Text; got != "ok" {
		t.Errorf("the pill reads %q, want the row's ok outcome", got)
	}
	if got := dom.descend(t, "systemupdates", 0, 2).Text; got != "8m" {
		t.Errorf("the time reads %q, want the last push's 8m when the row has no own end", got)
	}
}

// An ended roll no row carries yet reads "ended" in the pill, never a step
// name: no rows at all, a row for another commit, and a stub sha too short to
// be a commit (gt-avxdbf).
func TestSystemUpdatesBannerEndedRollWithNoRowReadsEnded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rows []any
	}{
		{"no rows", nil},
		{"another commit", []any{systemUpdateGoodRow()}},
		{"a stub sha", []any{map[string]any{"sha": "3333", "roll": map[string]any{"outcome": "ok"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			drawEndedRoll(t, dom, tc.rows...)
			pill := dom.descend(t, "systemupdates", 0, 1)
			if got := pill.Text; got != "ended" {
				t.Errorf("the pill reads %q, want ended", got)
			}
			if got := pill.Class; got != "tag" {
				t.Errorf("the pill wears %q, want no outcome class", got)
			}
		})
	}
}

// An idle Now draws no banner on a page that never saw a roll run.
func TestSystemUpdatesBannerDrawsNothingWhenNowIsIdle(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", bannerState(bannerNow{}, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "" {
		t.Fatalf("an idle Now drew the banner %q", got)
	}
	if got := dom.element(t, "systemupdates").Kids; got != 1 {
		t.Errorf("the pane drew %d lines, want the one commit row", got)
	}
}

// The pane draws one line under the sweep block (gt-logvfe): the newest green
// gastown sweep with its age and the commits main is ahead of it, 'no green
// sweep yet' without one, 'ahead unknown' at -1, and nothing while the sweep
// section cannot be read.

// lastGreenState is a reading whose section carries the ahead count and the last
// green sweep, which is nil when none is.
func lastGreenState(src systemUpdatesSources, green map[string]any, ahead int) map[string]any {
	st := systemUpdatesState(src, systemUpdateGoodRow())
	su := st["system_updates"].(map[string]any)
	if green != nil {
		su["last_green"] = green
	}
	su["ahead"] = ahead
	return st
}

// lastGreenAgo is a green sweep stamped two hours before now, as the feed sends it.
func lastGreenAgo() map[string]any {
	return map[string]any{"sha": "3333bbbb4444cccc", "at": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)}
}

// lastGreenLine is the line under the sweep block, or "" when the pane draws none.
func lastGreenLine(t *testing.T, dom *pageDOM) string {
	t.Helper()
	if first := dom.descend(t, "systemupdates", 0); strings.Contains(first.Class, "sulastgreen") {
		return first.Text
	}
	return ""
}

// The green sweep's sha is cut to eight characters and its age read off the stamp;
// an ahead count of -1 reads 'ahead unknown', never zero; no green sweep reads
// 'no green sweep yet'.
func TestSystemUpdatesLastGreenDrawsTheSweepAndAhead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		swept bool
		ahead int
		want  string
	}{
		{"a count", true, 3, `^last green 3333bbbb .+ \(2h00m ago\) · main is 3 commits ahead$`},
		{"unknown", true, -1, `^last green 3333bbbb .+ \(2h00m ago\) · ahead unknown$`},
		{"no green sweep", false, -1, `^no green sweep yet$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			// Stamped here, just before the render: a stamp made with the table
			// is a minute old or more by the time a loaded run gives this
			// subtest its turn (gt-6vvmxi).
			var green map[string]any
			if tc.swept {
				green = lastGreenAgo()
			}
			dom.call(t, "renderSystemUpdates", lastGreenState(systemUpdatesSources{}, green, tc.ahead))
			if got := lastGreenLine(t, dom); !regexp.MustCompile(tc.want).MatchString(got) {
				t.Errorf("the line reads %q, want it to match %s", got, tc.want)
			}
		})
	}
}

// A green sweep with no time draws its sha and count and no age, never a NaN.
func TestSystemUpdatesLastGreenWithoutATimeDrawsNoAge(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", lastGreenState(systemUpdatesSources{}, map[string]any{"sha": "3333bbbb4444cccc"}, 3))
	if got := lastGreenLine(t, dom); got != "last green 3333bbbb · main is 3 commits ahead" {
		t.Errorf("the line reads %q, want the sha and the count with no age", got)
	}
}

// A sweep section that cannot be read draws no line, with or without a green
// sweep, and a reading with no ahead count draws none either.
func TestSystemUpdatesLastGreenDrawsNothingWhenUnknown(t *testing.T) {
	t.Parallel()

	noAhead := lastGreenState(systemUpdatesSources{}, lastGreenAgo(), 0)
	delete(noAhead["system_updates"].(map[string]any), "ahead")
	for _, tc := range []struct {
		name string
		st   map[string]any
	}{
		{"a sweep source unknown with a green sweep", lastGreenState(systemUpdatesSources{sweepUnknown: true}, lastGreenAgo(), 3)},
		{"a sweep source unknown with no green sweep", lastGreenState(systemUpdatesSources{sweepUnknown: true}, nil, -1)},
		{"the section unavailable", lastGreenState(systemUpdatesSources{unavailable: true}, lastGreenAgo(), 3)},
		{"no ahead count", noAhead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderSystemUpdates", tc.st)
			if got := lastGreenLine(t, dom); got != "" {
				t.Errorf("the pane drew the line %q, want none", got)
			}
			if got := dom.element(t, "systemupdates").Kids; got != 1 {
				t.Errorf("the pane drew %d lines, want the one commit row", got)
			}
		})
	}
}

// sweepBlockSweep is a running sweep mid-cycle: integration finished in 1129 s
// and race running, with the shell tier's 300 s wall already spent on the host.
func sweepBlockSweep() map[string]any {
	return map[string]any{
		"rig": "gastown", "sha": "abcdef1234567890", "tier": "race", "elapsed_sec": 1500,
		"tiers":  []any{"integration", "race", "combo"},
		"stages": []any{map[string]any{"tier": "integration", "verdict": "GREEN", "passed": 12, "failed": 0, "secs": 1129}},
		"walls":  map[string]any{"shell": 300, "integration": 1129},
	}
}

// sweepLine is one line the pane drew under #systemupdates, its cells joined so
// a failure names the whole line: "tier | state | counts | time".
func sweepLine(t *testing.T, dom *pageDOM, line int) string {
	t.Helper()

	d := dom.descend(t, "systemupdates", line)
	cells := make([]string, d.Kids)
	for i := range cells {
		cells[i] = dom.descend(t, "systemupdates", line, i).Text
	}
	return strings.Join(cells, " | ")
}

// The sweep block (gt-iw2p6u) is a header naming the rig, the commit under
// sweep and the cycle's age with no tier beside it, then one line per tier of
// the cycle in order: the finished tier its verdict, counts and duration, the
// tier running now its own elapsed — the cycle's age less the walls the
// finished tiers took, the shell tier's included — and a dash for one waiting.
func TestSystemUpdatesSweepBlock(t *testing.T) {
	t.Parallel()

	t.Run("finished, running and waiting tiers", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderSystemUpdates", bannerState(bannerNow{sweep: sweepBlockSweep()}, systemUpdateGoodRow()))
		if got, want := sweepLine(t, dom, 0), "sweeping | gastown | @ abcdef12 | cycle 25m"; got != want {
			t.Errorf("the header reads %q, want %q", got, want)
		}
		if got, want := sweepLine(t, dom, 1), "integration | passed | 12 passed, 0 failed | 19m"; got != want {
			t.Errorf("the finished tier reads %q, want %q", got, want)
		}
		if got, want := sweepLine(t, dom, 2), "race | running | 71s"; got != want {
			t.Errorf("the running tier reads %q, want %q (1500s less shell 300s and integration 1129s)", got, want)
		}
		if got, want := sweepLine(t, dom, 3), "combo | –"; got != want {
			t.Errorf("the waiting tier reads %q, want %q", got, want)
		}
		if got := dom.element(t, "systemupdates").Kids; got != 5 {
			t.Errorf("the pane drew %d lines, want the header, three tiers and the one row", got)
		}
	})

	t.Run("the header carries no tier tag", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderSystemUpdates", bannerState(bannerNow{sweep: sweepBlockSweep()}, systemUpdateGoodRow()))
		head := dom.descend(t, "systemupdates", 0)
		for i := 0; i < head.Kids; i++ {
			if got := dom.descend(t, "systemupdates", 0, i).Class; strings.Contains(got, "tag") {
				t.Errorf("header cell %d wears %q, want no tier tag on the header", i, got)
			}
		}
	})

	t.Run("a shell tier finished with only its wall", func(t *testing.T) {
		t.Parallel()

		// The shell tier runs on the host before the job, so it has a wall and
		// no progress stage: it reads finished with its wall, never waiting.
		sweep := map[string]any{
			"rig": "gastown", "sha": "abcdef1234567890", "tier": "integration", "elapsed_sec": 600,
			"tiers": []any{"shell", "integration", "race"},
			"walls": map[string]any{"shell": 300},
		}
		dom := newPageDOM(t)
		dom.call(t, "renderSystemUpdates", bannerState(bannerNow{sweep: sweep}, systemUpdateGoodRow()))
		if got, want := sweepLine(t, dom, 1), "shell | finished | 5m"; got != want {
			t.Errorf("the shell tier reads %q, want %q", got, want)
		}
		if got, want := sweepLine(t, dom, 2), "integration | running | 5m"; got != want {
			t.Errorf("the running tier reads %q, want %q (600s less the shell's 300s)", got, want)
		}
		if got, want := sweepLine(t, dom, 3), "race | –"; got != want {
			t.Errorf("the waiting tier reads %q, want %q", got, want)
		}
	})

	t.Run("an old feed with no sha and no tiers", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderSystemUpdates", bannerState(bannerNow{sweep: bannerSweep()}, systemUpdateGoodRow()))
		if got, want := sweepLine(t, dom, 0), "sweeping | gastown | @ – | cycle 5m"; got != want {
			t.Errorf("the header reads %q, want %q", got, want)
		}
		if got := dom.element(t, "systemupdates").Kids; got != 2 {
			t.Errorf("the pane drew %d lines, want the header and the one row and no tier lines", got)
		}
	})

	t.Run("an empty sha", func(t *testing.T) {
		t.Parallel()

		sweep := sweepBlockSweep()
		sweep["sha"] = ""
		dom := newPageDOM(t)
		dom.call(t, "renderSystemUpdates", bannerState(bannerNow{sweep: sweep}, systemUpdateGoodRow()))
		if got, want := sweepLine(t, dom, 0), "sweeping | gastown | @ – | cycle 25m"; got != want {
			t.Errorf("the header reads %q, want a dash for the empty sha", got)
		}
	})

	t.Run("an unknown sweep", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderSystemUpdates", bannerSweepUnknownState(systemUpdateGoodRow()))
		if got := dom.element(t, "systemupdates").Kids; got != 1 {
			t.Errorf("the pane drew %d lines, want the one commit row and no sweep block", got)
		}
	})
}
