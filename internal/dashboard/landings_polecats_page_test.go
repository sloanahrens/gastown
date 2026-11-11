package dashboard

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// The Polecats section is the seats table, drawn in the Landings body between
// its figures and notes and its landing rows (gt-xdrt5y). A landings redraw
// draws around it and never moves it, so its fold and its text stay put.

// checkLandingsIsLastBlock holds the Landings section as the last block of the
// page. Its body nests the Polecats section, the one section that may follow the
// Landings opening tag; any other section after it is a layout mistake.
func checkLandingsIsLastBlock(t *testing.T, page string) {
	t.Helper()

	at := strings.Index(page, `<section id="trendsec"`)
	if at < 0 {
		t.Fatal("the page has no Landings section")
	}
	tail := page[at+len("<section"):]
	end := strings.Index(tail, "<script>")
	if end < 0 {
		t.Fatal("index.html has no script after the Landings section")
	}
	body := tail[:end]
	if n := strings.Count(body, "<section"); n != 1 || !strings.Contains(body, `<section id="polecatssec" data-fold="polecats">`) {
		t.Errorf("%d sections follow the Landings section's opening tag, want only its nested Polecats section: it is not the last block", n)
	}
}

// landingsBodyKinds reads the Landings body's children in the order the page
// drew them: a figure, the Polecats section, or the landing rows. The stub keeps
// the children the page appended, so this is the order a reader sees.
func landingsBodyKinds(t *testing.T, dom *pageDOM) string {
	t.Helper()

	v, err := dom.vm.RunString(`(function () {
		var pol = document.getElementById("polecatssec");
		return document.getElementById("trend").children.__arr.map(function (c) {
			if (c === pol) return "polecats";
			return __find(c, ".ltable") ? "rows" : "figure";
		}).join(",");
	})()`)
	if err != nil {
		t.Fatalf("reading the Landings body's order: %v", err)
	}
	return v.String()
}

// landingsSeatsState is a reading with seats, one working and one idle, and a
// landings window with one hour, two landings and a stage and load reading, so
// the body has figures, the seats and rows all drawn.
func landingsSeatsState(t *testing.T) map[string]any {
	t.Helper()

	now := time.Now().UTC()
	st := workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "one Work panel replaces Polecats", "deepseek-flash", 12*time.Minute),
		seat("gastown", "furiosa", "idle", "", "", "deepseek-flash", 0),
	}, nil)
	st["trend"] = map[string]any{
		"hours":  []any{map[string]any{"hour": now.Truncate(time.Hour).Format(time.RFC3339), "landed": 2, "rejected": 0, "load_avg": 1.5}},
		"stages": []any{map[string]any{"gate_secs": 120, "om_secs": 60, "lint_secs": 5}},
		"recent": []any{
			map[string]any{"at": now.Add(-5 * time.Minute).Format(time.RFC3339), "bead": "gt-a1", "rig": "gastown", "outcome": "landed", "polecat": "dementus", "gate_secs": 120, "om_secs": 60, "lint_secs": 5},
			map[string]any{"at": now.Add(-9 * time.Minute).Format(time.RFC3339), "bead": "gt-a2", "rig": "gastown", "outcome": "landed", "polecat": "furiosa", "gate_secs": 120, "om_secs": 60, "lint_secs": 5},
		},
	}
	return st
}

// TestPolecatsIsInLandingsBetweenItsFiguresAndRows draws the Landings body over a
// reading with figures, seats and landings, then reads the body's order and the
// seat rows: the figures first, the Polecats section, then the landing rows, and
// the seats drawn in the table's five columns with the seat's header summary.
func TestPolecatsIsInLandingsBetweenItsFiguresAndRows(t *testing.T) {
	t.Parallel()

	st := landingsSeatsState(t)
	dom := newPageDOM(t)
	renderWorkReading(t, dom, st)

	kinds := landingsBodyKinds(t, dom)
	if !strings.HasPrefix(kinds, "figure") || !strings.Contains(kinds, ",polecats,rows") || !strings.HasSuffix(kinds, ",polecats,rows") {
		t.Fatalf("the Landings body reads %q, want the figures, then the Polecats section, then the rows", kinds)
	}
	if strings.Count(kinds, "polecats") != 1 {
		t.Errorf("the Landings body draws %d Polecats sections, want one", strings.Count(kinds, "polecats"))
	}

	rows := workRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the Polecats section drew %d rows (%v), want a header and two seats", len(rows), rows)
	}
	if got := strings.Join(rows[0], " "); got != "Polecat Model Bead State Age" {
		t.Errorf("the seat header reads %q, want the same columns as before", got)
	}
	if rows[1][0] != "dementus" || rows[1][3] != "working" {
		t.Errorf("the working seat's row reads %v, want the polecat and its state", rows[1])
	}

	fold := dom.fold(t, "polecats")
	if fold.Collapsed {
		t.Error("the Polecats section starts collapsed, want it open for a reader with no saved choice")
	}
	if fold.Summary != "1 working · 1 idle" {
		t.Errorf("the Polecats header reads %q, want the summary the old pane header showed", fold.Summary)
	}
}

// TestPolecatsSeatsDrawInLandingsWithoutAnyLanding: a seats reading with no
// landing or queued bead is reason to show the Landings block, so the seats are
// drawn there and not lost behind a hidden section.
func TestPolecatsSeatsDrawInLandingsWithoutAnyLanding(t *testing.T) {
	t.Parallel()

	st := workState([]any{
		seat("gastown", "dementus", "working", "gt-vcr3z", "the wait", "deepseek-flash", 12*time.Minute),
	}, nil)
	dom := newPageDOM(t)
	callWork(t, dom, st)
	dom.call(t, "renderTrend", st)

	if dom.element(t, "trendsec").Hidden {
		t.Error("the Landings block is hidden with a seats reading, want it shown so the seats are drawn in it")
	}
	if rows := workRows(t, dom); len(rows) != 2 {
		t.Errorf("the Polecats section drew %d rows, want a header and the one seat", len(rows))
	}
}

// TestLandingsStaysHiddenWithNothingToDraw: no seats reading, no landing and no
// queued bead leaves the block hidden, as it was before the seats moved in.
func TestLandingsStaysHiddenWithNothingToDraw(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderTrend", map[string]any{})
	if !dom.element(t, "trendsec").Hidden {
		t.Error("the Landings block is shown with no seats, no landing and no queue, want it hidden")
	}
}

// TestThePolecatsSectionKeepsItsFoldAcrossALandingsRedraw: the reader closes the
// Polecats section, the Landings body redraws over a new reading, and the section
// stays closed, still in its place, with the choice saved for a reload.
func TestThePolecatsSectionKeepsItsFoldAcrossALandingsRedraw(t *testing.T) {
	t.Parallel()

	st := landingsSeatsState(t)
	dom := newPageDOM(t)
	renderWorkReading(t, dom, st)
	dom.press(t, "polecats", "click")
	wantFold(t, dom.fold(t, "polecats"), "polecats", true)

	st["trend"].(map[string]any)["hours"].([]any)[0].(map[string]any)["landed"] = 3
	renderWorkReading(t, dom, st)
	wantFold(t, dom.fold(t, "polecats"), "polecats", true)
	if kinds := landingsBodyKinds(t, dom); !strings.Contains(kinds, ",polecats,rows") {
		t.Errorf("after the redraw the Landings body reads %q, want the Polecats section still between the figures and the rows", kinds)
	}

	reloaded := newPageDOMStoring(t, dom.storage(t))
	wantFold(t, reloaded.fold(t, "polecats"), "polecats", true)
}

// TestTheStandalonePolecatsPaneIsGone: the page carries one Polecats section, and
// it sits in the Landings block, not in a column of the grid.
func TestTheStandalonePolecatsPaneIsGone(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if n := strings.Count(page, `data-fold="polecats"`); n != 1 {
		t.Fatalf("the page folds %d sections under polecats, want one", n)
	}
	grid := page[strings.Index(page, `<div class="grid">`):strings.Index(page, `<section id="trendsec"`)]
	if strings.Contains(grid, `id="polecats"`) || strings.Contains(grid, `data-fold="polecats"`) {
		t.Error("the grid still carries a standalone Polecats pane")
	}
	dom := newPageDOM(t)
	keys := dom.foldKeys(t)
	n := 0
	for _, k := range keys {
		if k == "polecats" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the page folds %d sections under polecats, want one", n)
	}
}

// TestThePolecatsSectionCollapsesAndIsRememberedInLandings reads the section's
// fold after a reader closes it and a reload: the same key the other sections
// use, saved in gt-fold, so the choice holds across a reload.
func TestThePolecatsSectionCollapsesAndIsRememberedInLandings(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.press(t, "polecats", "click")
	wantFold(t, dom.fold(t, "polecats"), "polecats", true)
	if got := dom.storage(t)["gt-fold"]; !regexp.MustCompile(`"polecats":\s*true`).MatchString(got) {
		t.Errorf("gt-fold holds %q after the reader closed the Polecats section, want polecats true", got)
	}
	reloaded := newPageDOMStoring(t, dom.storage(t))
	wantFold(t, reloaded.fold(t, "polecats"), "polecats", true)
}
