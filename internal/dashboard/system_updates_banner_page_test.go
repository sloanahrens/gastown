package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// The pane leads with a banner while a gt update roll runs (gt-o8m8ho): its
// step, elapsed and the builds it moves between; an ended roll stays ten minutes.

func bannerRoll() map[string]any {
	return map[string]any{"run_id": "run-7", "step": "install", "elapsed_sec": 480, "from": "1111aaaa2222", "to": "3333bbbb4444"}
}

// bannerState is one reading of the section: rows, and Now's roll unless nil.
func bannerState(roll map[string]any, rows ...any) map[string]any {
	st := systemUpdatesState(systemUpdatesSources{}, rows...)
	if roll != nil {
		st["system_updates"].(map[string]any)["now"] = map[string]any{"roll": roll}
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
	dom.call(t, "renderSystemUpdates", bannerState(bannerRoll(), systemUpdateGoodRow()))
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
	dom.call(t, "renderSystemUpdates", bannerState(roll, systemUpdateGoodRow()))
	if got := dom.descend(t, "systemupdates", 0, 3).Text; got != "– → 3333bbbb4444" {
		t.Errorf("a roll with no from build reads %q, want a dash", got)
	}
}

// An ended roll's banner is dated by the first push that no longer names it,
// even one that skips the pane's redraw, and stays ten minutes to the
// millisecond: exactly ten minutes is still there, a millisecond later gone.
func TestSystemUpdatesBannerKeepsAnEndedRollTenMinutes(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.press(t, "systemupdates", "click")
	drawState(t, dom, bannerState(nil, systemUpdateGoodRow()))                           // the pane's recorded key
	dom.call(t, "renderSystemUpdates", bannerState(bannerRoll(), systemUpdateGoodRow())) // the running roll
	drawState(t, dom, bannerState(nil, systemUpdateGoodRow()))                           // same key: the push skips, still dating the end
	stay := pageNumber(t, dom, "SYSTEM_UPDATES_BANNER_STAY_MS")
	pageExpr(t, dom, "__clock = "+strconv.Itoa(stay))
	dom.call(t, "renderSystemUpdates", bannerState(nil, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "roll ended" {
		t.Fatalf("at exactly ten minutes the banner reads %q, want it still there", got)
	}
	pageExpr(t, dom, "__clock = "+strconv.Itoa(stay+1))
	dom.call(t, "renderSystemUpdates", bannerState(nil, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "" {
		t.Errorf("a millisecond past ten minutes the banner reads %q, want it gone", got)
	}
}

// An idle Now draws no banner on a page that never saw a roll run.
func TestSystemUpdatesBannerDrawsNothingWhenNowIsIdle(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", bannerState(nil, systemUpdateGoodRow()))
	if got := paneBanner(t, dom); got != "" {
		t.Fatalf("an idle Now drew the banner %q", got)
	}
	if got := dom.element(t, "systemupdates").Kids; got != 1 {
		t.Errorf("the pane drew %d lines, want the one commit row", got)
	}
}
