package dashboard

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Mango panel is folded until the operator opens it, like the JEV panel
// above it: its markup carries the attribute the fold reads on first load, and
// the fold keeps no saved choice of its own, so a reload starts it folded too.
func TestMangoPanelStartsFolded(t *testing.T) {
	t.Parallel()

	tag := mangoSectionTag(t)
	if !strings.Contains(tag, " data-default-collapsed") || !strings.Contains(tag, " hidden") {
		t.Errorf("the Mango section %q does not start folded on first load", tag)
	}
	if !strings.Contains(pageFunc(t, "setupCollapse"), `sec.hasAttribute("data-default-collapsed")`) {
		t.Error("setupCollapse no longer folds a pane that opts in with data-default-collapsed")
	}
}

// A hub with no Mango reader sends no mango key, and the panel stays out of the
// page; once the hub reads one, the panel shows. renderAll is what draws it.
func TestMangoPanelIsHiddenUntilTheHubReadsIt(t *testing.T) {
	t.Parallel()

	if !strings.Contains(pageFunc(t, "renderAll"), "renderMango(state)") {
		t.Fatal("renderAll does not draw the Mango panel")
	}

	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{})
	if !dom.element(t, "mangosec").Hidden {
		t.Error("the Mango panel shows with no reading from the hub")
	}

	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh")})
	if dom.element(t, "mangosec").Hidden {
		t.Error("the Mango panel stays hidden once the hub has read it")
	}
}

// A fresh panel's title line carries the landings, the snapshot hours, the
// analysis age and the verdict counts as chips; its table has a collection row
// saying the state, then one row per target. A skipped target says it had too
// few positives to check, in dashes rather than figures.
func TestMangoFreshPanelDrawsItsTitleLineAndTargets(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(mangoNow.UnixMilli(), 10))
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus and attempts in q4", 96, 0.42, 0.25, MangoEarning),
		mangoPageTarget("reached_overrun_stop", 12, true, "", 0, 0, 0.03, MangoNotYet),
	)})

	note := dom.element(t, "mangonote")
	if note.Class != "r" {
		t.Errorf("a fresh panel's title is %q, want the plain title class", note.Class)
	}
	for _, want := range []string{"412 landings", "168 snapshot hours", "analysed 2h00m ago", "fresh", "1 earning", "0 no-signal", "1 not-yet"} {
		if !strings.Contains(note.Text, want) {
			t.Errorf("the title line %q has no %q", note.Text, want)
		}
	}
	if dom.element(t, "mangosec").Hidden {
		t.Error("the panel is hidden while it holds a fresh reading")
	}

	if n := dom.descend(t, "mango", 0, 1).Kids; n != 3 {
		t.Fatalf("the table has %d rows, want the collection row and two targets", n)
	}
	for _, tc := range []struct {
		row, col int
		want     string
	}{
		{0, 0, "collection"},
		{0, 5, "fresh"},
		{1, 0, "rejected_before_landing"},
		{1, 1, "earning"},
		{1, 2, "48"},
		{1, 3, "42.0% of 96"},
		{1, 4, "25.0%"},
		{1, 5, "model = opus and attempts in q4"},
		{2, 0, "reached_overrun_stop"},
		{2, 1, "not-yet"},
		{2, 2, "12"},
		{2, 3, "-"},
		{2, 4, "-"},
		{2, 5, "too few positives to check"},
	} {
		if got := mangoCell(t, dom, tc.row, tc.col).Text; got != tc.want {
			t.Errorf("row %d cell %d = %q, want %q", tc.row, tc.col, got, tc.want)
		}
	}
	if class := dom.descend(t, "mango", 0, 1, 1).Class; class != "" {
		t.Errorf("a fresh target row is drawn as %q, want no greying", class)
	}
}

// A stale panel says so in its title, and greys the collection row and every
// target: the figures are the last analysis's, and the page says how old they
// are rather than presenting them as current.
func TestMangoStalePanelSaysSoAndGreysItsRows(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(mangoNow.UnixMilli(), 10))
	stale := mangoPageState(MangoStale, "stale",
		mangoPageTarget("manual_rescue", 37, false, "attempts in q3..q4", 54, 0.11, 0.09, MangoNoSignal))
	stale["generated_at"] = mangoNow.Add(-72 * time.Hour).Format(time.RFC3339)
	dom.call(t, "renderMango", map[string]any{"mango": stale})

	if note := dom.element(t, "mangonote"); note.Class != "r warnc" || !strings.Contains(note.Text, "stale") {
		t.Errorf("a stale panel's title is %q (%q), want the warning class and the word stale", note.Text, note.Class)
	}
	if got := dom.descend(t, "mango", 0, 1, 0).Class; got != "jvstale" {
		t.Errorf("the collection row of a stale panel is drawn as %q, want jvstale", got)
	}
	if got := mangoCell(t, dom, 0, 5).Text; got != "stale" {
		t.Errorf("the collection row says %q, want stale", got)
	}
	if got := dom.descend(t, "mango", 0, 1, 1).Class; got != "jvstale" {
		t.Errorf("a target row of a stale panel is drawn as %q, want jvstale", got)
	}
}

// A missing panel says it has not been collected yet, in its title and in the
// collection row, and draws no target rows.
func TestMangoMissingPanelSaysNotCollectedYet(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoMissing, mangoNoPanel)})

	if note := dom.element(t, "mangonote"); note.Text != mangoNoPanel || note.Class != "r" {
		t.Errorf("a missing panel's title is %q (%q), want %q", note.Text, note.Class, mangoNoPanel)
	}
	if n := dom.descend(t, "mango", 0, 1).Kids; n != 1 {
		t.Errorf("a missing panel draws %d rows, want the collection row alone", n)
	}
	if got := mangoCell(t, dom, 0, 5).Text; got != mangoNoPanel {
		t.Errorf("the collection row says %q, want %q", got, mangoNoPanel)
	}
}

// A panel the reader could not use names the error in its title, marked in the
// error colour, and draws no target rows: a last analysis the reader could not
// read is not a table of current verdicts. The error's words come from the
// reader, so the page shows them as they are.
func TestMangoErrorPanelNamesTheErrorAndDrawsNoRows(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoError, "the panel is not valid JSON",
		mangoPageTarget("manual_rescue", 37, false, "attempts in q3", 54, 0.11, 0.09, MangoNoSignal))})

	if note := dom.element(t, "mangonote"); note.Text != "the panel is not valid JSON" || note.Class != "r badc" {
		t.Errorf("an error panel's title is %q (%q), want the error named in the error class", note.Text, note.Class)
	}
	if n := dom.descend(t, "mango", 0, 1).Kids; n != 1 {
		t.Errorf("an error panel draws %d rows, want the collection row alone", n)
	}
	if got := dom.descend(t, "mango", 0, 1, 0).Class; got != "" {
		t.Errorf("the collection row of an error panel is drawn as %q", got)
	}
	if got := mangoCell(t, dom, 0, 5); got.Text != "the panel is not valid JSON" || got.Class != "badc" {
		t.Errorf("the collection row says %q (%q), want the error in the error class", got.Text, got.Class)
	}
}

// A state the page does not know is drawn as the panel's own note in the plain
// title colour, with no target rows: the page does not make a reading out of a
// state it cannot name.
func TestMangoUnknownStateShowsItsNotePlainly(t *testing.T) {
	t.Parallel()

	const note = "the panel state is not one this page knows"
	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState("indeterminate", note,
		mangoPageTarget("manual_rescue", 37, false, "attempts in q3", 54, 0.11, 0.09, MangoNoSignal))})

	if got := dom.element(t, "mangonote"); got.Text != note || got.Class != "r" {
		t.Errorf("an unknown state's title is %q (%q), want the note in the plain title class", got.Text, got.Class)
	}
	if n := dom.descend(t, "mango", 0, 1).Kids; n != 1 {
		t.Errorf("an unknown state draws %d rows, want the collection row alone", n)
	}
	if got := mangoCell(t, dom, 0, 5); got.Text != note || got.Class != "empty" {
		t.Errorf("the collection row says %q (%q), want the note in the empty class", got.Text, got.Class)
	}
}

// A panel that carries no generation time dates its reading as unknown rather
// than leaving the age out: an undated analysis would read as a current one.
func TestMangoFreshPanelWithoutAGenerationTimeSaysTheAgeIsUnknown(t *testing.T) {
	t.Parallel()

	m := mangoPageState(MangoFresh, "fresh")
	delete(m, "generated_at")
	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{"mango": m})

	note := dom.element(t, "mangonote")
	if !strings.Contains(note.Text, "analysis age unknown") {
		t.Errorf("the title line %q does not say the analysis age is unknown", note.Text)
	}
	if strings.Contains(note.Text, "analysed") {
		t.Errorf("the title line %q dates an analysis that carries no generation time", note.Text)
	}
}

// A verdict the page does not know and a rate the panel did not report are drawn
// as the panel's word, dim, and as a dash: an unknown verdict never wears a
// counted verdict's colour, and a missing rate never reads as zero.
func TestMangoUnknownVerdictAndMissingRateAreNotReadings(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	unmeasured := mangoPageTarget("manual_rescue", 37, false, "attempts in q3", 54, 0.11, 0.09, "unmeasured")
	unrated := map[string]any{
		"target":       "reached_overrun_stop",
		"positives":    12,
		"skipped":      false,
		"best_box":     "model = opus",
		"holdout_n":    96,
		"holdout_rate": nil,
		"baseline":     nil,
		"verdict":      MangoNoSignal,
	}
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", unmeasured, unrated)})

	chip := dom.descend(t, "mango", 0, 1, 1, 1, 0)
	if chip.Text != "unmeasured" {
		t.Errorf("the chip says %q, want the verdict word the panel gave", chip.Text)
	}
	if chip.Class != "jvchip jv-notyet" {
		t.Errorf("an unknown verdict is drawn as %q, want the dim chip", chip.Class)
	}
	if !strings.Contains(chip.Title, "does not know") {
		t.Errorf("an unknown verdict's tooltip is %q, want it to say the page does not know the verdict", chip.Title)
	}
	for _, col := range []int{3, 4} {
		if got := mangoCell(t, dom, 2, col).Text; got != "-" {
			t.Errorf("a missing rate in column %d reads %q, want a dash", col, got)
		}
	}
}

// The best box and the target name are text the panel writes into the page: a
// box holding markup is drawn as those characters, as one text run with no
// element inside it, so nothing it says can run or restyle the page.
func TestMangoBestBoxWithMarkupRendersAsText(t *testing.T) {
	t.Parallel()

	const name = "<i>rejected</i>"
	const box = `<img src=x onerror="alert(1)"><b>model = opus</b>`
	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh",
		mangoPageTarget(name, 48, false, box, 96, 0.42, 0.25, MangoEarning))})

	for col, want := range map[int]string{0: name, 5: box} {
		got := mangoCell(t, dom, 1, col)
		if got.Text != want {
			t.Errorf("cell %d draws %q, want the raw text %q", col, got.Text, want)
		}
		if got.Kids != 0 {
			t.Errorf("cell %d holds %d elements, want its text alone", col, got.Kids)
		}
	}
}

// mangoPageState is the panel the hub hands the page: the Mango the reader made,
// in the keys the state payload carries it under. Its targets are the given ones,
// and a fresh or stale panel is generated two hours before the test's clock.
func mangoPageState(state, note string, targets ...map[string]any) map[string]any {
	m := map[string]any{
		"at":             mangoNow.Format(time.RFC3339),
		"state":          state,
		"note":           note,
		"items":          412,
		"snapshot_hours": 168,
		"targets":        targets,
		"counts":         map[string]any{"earning": 1, "no_signal": 0, "not_yet": 1},
	}
	if state == MangoFresh || state == MangoStale {
		m["generated_at"] = mangoNow.Add(-2 * time.Hour).Format(time.RFC3339)
	}
	return m
}

// mangoPageTarget is one target as the hub hands the page: the MangoTarget keys.
func mangoPageTarget(target string, positives int, skipped bool, box string, holdoutN int, holdoutRate, baseline float64, verdict string) map[string]any {
	return map[string]any{
		"target":       target,
		"positives":    positives,
		"skipped":      skipped,
		"best_box":     box,
		"holdout_n":    holdoutN,
		"holdout_rate": holdoutRate,
		"baseline":     baseline,
		"verdict":      verdict,
	}
}

// mangoSectionTag returns the opening tag of the markup's #mangosec section, so a
// test can read the attributes it opts in with without pinning their order.
func mangoSectionTag(t *testing.T) string {
	t.Helper()

	page := string(indexHTML)
	at := strings.Index(page, `id="mangosec"`)
	if at < 0 {
		t.Fatal("index.html has no #mangosec section")
	}
	start := strings.LastIndex(page[:at], "<section")
	if start < 0 {
		t.Fatal("index.html has no <section> holding #mangosec")
	}
	end := strings.Index(page[at:], ">")
	if end < 0 {
		t.Fatal("index.html's #mangosec section has no opening tag")
	}
	return page[start : at+end+1]
}

// mangoCell reads one cell of the Mango table: the row at index row of its body,
// counting the collection row as row 0, and the cell at column col.
func mangoCell(t *testing.T, dom *pageDOM, row, col int) drawn {
	t.Helper()

	return dom.descend(t, "mango", 0, 1, row, col)
}
