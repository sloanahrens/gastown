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
// saying the state, then one row per checkable target. A target the panel could
// not check leaves the table and becomes one line under it.
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
	for _, want := range []string{"412 landings", "168 snapshot hours", "analysed 2h00m ago", "fresh", "1 earning", "0 no-signal", "1 not checkable"} {
		if !strings.Contains(note.Text, want) {
			t.Errorf("the title line %q has no %q", note.Text, want)
		}
	}
	if dom.element(t, "mangosec").Hidden {
		t.Error("the panel is hidden while it holds a fresh reading")
	}

	if n := dom.descend(t, "mango", 0, 1).Kids; n != 2 {
		t.Fatalf("the table has %d rows, want the collection row and the one checkable target", n)
	}
	for _, tc := range []struct {
		row, col int
		want     string
	}{
		{0, 0, "collection"},
		{0, 5, "fresh"},
		{1, 0, "rejected_before_landing"},
		{1, 5, "model = opus and attempts in q4"},
	} {
		if got := mangoCell(t, dom, tc.row, tc.col).Text; got != tc.want {
			t.Errorf("row %d cell %d = %q, want %q", tc.row, tc.col, got, tc.want)
		}
	}
	if got := mangoCell(t, dom, 1, 1).Text; got != "earning" {
		t.Errorf("the target's verdict chip reads %q, want the panel's word", got)
	}
	if got := mangoCell(t, dom, 1, 2).Text; !strings.Contains(got, "42.0% of 96") || !strings.Contains(got, "vs 25.0% across all recent landings") {
		t.Errorf("the target's rate cell reads %q, want the rate of its group against the baseline", got)
	}
	if class := dom.descend(t, "mango", 0, 1, 1).Class; class != "" {
		t.Errorf("a fresh target row is drawn as %q, want no greying", class)
	}

	line := dom.descend(t, "mango", 1)
	for _, want := range []string{"not checkable", "reached_overrun_stop", "12 positives", "too few positives to check"} {
		if !strings.Contains(line.Text, want) {
			t.Errorf("the not-checkable line %q has no %q", line.Text, want)
		}
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
	rates := mangoCell(t, dom, 2, 2)
	if big := dom.descend(t, "mango", 0, 1, 2, 2, 0).Text; big != "-" {
		t.Errorf("a missing rate reads %q, want a dash", big)
	}
	if strings.Contains(rates.Text, "0.0%") {
		t.Errorf("a missing rate is drawn as %q, want no figure the analysis never gave", rates.Text)
	}
	if vs := dom.descend(t, "mango", 0, 1, 2, 2, 1).Text; vs != "vs - across all recent landings" {
		t.Errorf("a missing baseline reads %q, want a dash", vs)
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

// A replicating target draws its plain label, a thin chip naming the holdout its
// rate rests on, the rate against the baseline with its lift and the group's
// count, an interval bar, a chip for each condition that narrows the group, and
// a fold holding every condition including the one that does not narrow.
func TestMangoReplicatingThinTargetDrawsItsNumbers(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	target := mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 36, 0.417, 0.198, MangoEarning)
	target["label"] = "Landings rejected before they land"
	target["holdout_positives"] = 15
	target["ci_low"] = 0.271
	target["ci_high"] = 0.578
	target["thin"] = true
	target["conditions"] = []any{
		map[string]any{"text": "model = opus", "share": 0.12, "narrows": true},
		map[string]any{"text": "attempts in q4", "share": 0.18, "narrows": true},
		map[string]any{"text": "rig = gastown", "share": 0.71, "narrows": false},
	}
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	if n := dom.descend(t, "mango", 0, 1).Kids; n != 2 {
		t.Fatalf("the table has %d rows, want the collection row and the target", n)
	}
	if got := mangoCell(t, dom, 1, 0).Text; got != "Landings rejected before they land" {
		t.Errorf("the target's title is %q, want the panel's plain label", got)
	}
	thin := dom.descend(t, "mango", 0, 1, 1, 1, 1)
	if thin.Class != "jvchip jvthin" {
		t.Errorf("the thin chip is drawn as %q, want the dashed chip class", thin.Class)
	}
	if !strings.Contains(thin.Text, "thin") || !strings.Contains(thin.Text, "n=36") {
		t.Errorf("the thin chip %q does not name thin and the holdout it rests on", thin.Text)
	}
	rates := mangoCell(t, dom, 1, 2)
	for _, want := range []string{"41.7%", "vs 19.8% across all recent landings", "2.1x as likely", "15 of 36 recent landings in the group"} {
		if !strings.Contains(rates.Text, want) {
			t.Errorf("the rate cell %q has no %q", rates.Text, want)
		}
	}

	bar := dom.descend(t, "mango", 0, 1, 1, 3, 0)
	if bar.Kids != 4 {
		t.Fatalf("the interval bar holds %d parts, want the track, the baseline marker, the bar and the rate dot", bar.Kids)
	}
	for i, class := range []string{"mgtrack", "mgbase", "mgci", "mgdot"} {
		if got := dom.descend(t, "mango", 0, 1, 1, 3, 0, i).Class; got != class {
			t.Errorf("the interval bar's part %d is drawn as %q, want %q", i, got, class)
		}
	}

	evidence := dom.descend(t, "mango", 0, 1, 1, 4)
	if evidence.Kids != 3 {
		t.Fatalf("the evidence cell holds %d parts, want two narrowing chips and the fold", evidence.Kids)
	}
	for i, want := range []string{"model = opus 12.0% of landings", "attempts in q4 18.0% of landings"} {
		chip := dom.descend(t, "mango", 0, 1, 1, 4, i)
		if chip.Text != want {
			t.Errorf("the narrowing chip %d reads %q, want %q", i, chip.Text, want)
		}
		if chip.Class != "jvchip mgcondchip" {
			t.Errorf("the narrowing chip %d is drawn as %q", i, chip.Class)
		}
	}
	if got := dom.descend(t, "mango", 0, 1, 1, 4, 2, 0).Text; got != "how it was found" {
		t.Errorf("the fold is named %q, want how it was found", got)
	}
	list := dom.descend(t, "mango", 0, 1, 1, 4, 2, 1)
	if list.Kids != 3 {
		t.Fatalf("the fold holds %d conditions, want all three", list.Kids)
	}
	for i, want := range []string{"model = opus", "attempts in q4", "rig = gastown"} {
		if got := dom.descend(t, "mango", 0, 1, 1, 4, 2, 1, i).Text; !strings.Contains(got, want) || !strings.Contains(got, "of landings") {
			t.Errorf("the fold's condition %d reads %q, want %q and its share", i, got, want)
		}
	}
	last := dom.descend(t, "mango", 0, 1, 1, 4, 2, 1, 2)
	if last.Class != "mgcond jvstale" {
		t.Errorf("the condition that does not narrow is drawn as %q, want it greyed", last.Class)
	}
	if !strings.Contains(last.Text, "doesn't narrow") {
		t.Errorf("the condition that does not narrow reads %q, want it marked", last.Text)
	}
}

// A target the panel could not check is one line under the table, with the
// not-checkable chip, its plain label, its positives and the reason in words,
// and it is no table row.
func TestMangoNotCheckableTargetDrawsOneLineAndNoRow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	target := mangoPageTarget("reached_overrun_stop", 12, true, "", 0, 0, 0.03, MangoNotYet)
	target["label"] = "Beads stopped by the overrun guard"
	target["reason"] = "no_holdout_positives"
	target["last_positive"] = "2026-10-19"
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	if n := dom.descend(t, "mango", 0, 1).Kids; n != 1 {
		t.Fatalf("the table has %d rows, want the collection row alone: a not-checkable target is no row", n)
	}
	line := dom.descend(t, "mango", 1)
	if line.Class != "mgnot" {
		t.Errorf("the not-checkable line is drawn as %q, want the line class", line.Class)
	}
	if chip := dom.descend(t, "mango", 1, 0); chip.Text != "not checkable" || chip.Class != "jvchip jv-notyet" {
		t.Errorf("the line's chip is %q (%q), want the dim not-checkable chip", chip.Text, chip.Class)
	}
	for _, want := range []string{"Beads stopped by the overrun guard", "12 positives", "no positives in the recent landings to check it on, its last positive was 2026-10-19"} {
		if !strings.Contains(line.Text, want) {
			t.Errorf("the not-checkable line %q has no %q", line.Text, want)
		}
	}
}

// The title line counts each verdict in the panel's counts: the two verdicts it
// calls by name and the not-yet targets it calls not checkable.
func TestMangoTitleLineCountsVerdictsForAMixedPanel(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	m := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning),
		mangoPageTarget("manual_rescue", 37, false, "attempts in q3", 54, 0.11, 0.09, MangoEarning),
		mangoPageTarget("reached_overrun_stop", 12, false, "model = haiku", 50, 0.08, 0.04, MangoNoSignal),
		mangoPageTarget("slow_landing", 3, true, "", 0, 0, 0.02, MangoNotYet),
		mangoPageTarget("failed_gate", 2, true, "", 0, 0, 0.01, MangoNotYet),
		mangoPageTarget("parked_bead", 1, true, "", 0, 0, 0.05, MangoNotYet),
	)
	m["counts"] = map[string]any{"earning": 2, "no_signal": 1, "not_yet": 3}
	dom.call(t, "renderMango", map[string]any{"mango": m})

	note := dom.element(t, "mangonote")
	for _, want := range []string{"2 earning", "1 no-signal", "3 not checkable"} {
		if !strings.Contains(note.Text, want) {
			t.Errorf("the title line %q has no %q", note.Text, want)
		}
	}
	if strings.Contains(note.Text, "not-yet") {
		t.Errorf("the title line %q counts not-yet targets by a word the panel does not use", note.Text)
	}
}

// An older panel carries none of the newer target keys and still draws one row
// per target with its id, its rate of its group, its baseline, its best box and
// no condition chips.
func TestMangoOldPanelKeepsItsColumns(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus and attempts in q4", 96, 0.42, 0.25, MangoEarning),
		mangoPageTarget("reached_overrun_stop", 12, false, "model = haiku", 50, 0.08, 0.04, MangoNoSignal),
	)})

	if n := dom.descend(t, "mango", 0, 1).Kids; n != 3 {
		t.Fatalf("the table has %d rows, want the collection row and two targets", n)
	}
	for _, tc := range []struct {
		row, col int
		want     string
	}{
		{1, 0, "rejected_before_landing"},
		{2, 0, "reached_overrun_stop"},
		{1, 5, "model = opus and attempts in q4"},
		{2, 5, "model = haiku"},
	} {
		if got := mangoCell(t, dom, tc.row, tc.col).Text; got != tc.want {
			t.Errorf("row %d cell %d = %q, want %q", tc.row, tc.col, got, tc.want)
		}
	}
	if got := mangoCell(t, dom, 1, 2).Text; !strings.Contains(got, "42.0% of 96") || !strings.Contains(got, "vs 25.0% across all recent landings") {
		t.Errorf("the target's rate cell reads %q, want the rate of its group against the baseline", got)
	}
	if n := dom.descend(t, "mango", 0, 1, 1, 4).Kids; n != 1 {
		t.Errorf("an old panel's evidence cell holds %d parts, want the fold alone and no chips", n)
	}
	if got := dom.descend(t, "mango", 0, 1, 1, 4, 0, 1, 0).Text; got != "model = opus and attempts in q4" {
		t.Errorf("the fold holds %q, want the old best box", got)
	}
}

// A condition's text is the panel's own string: markup in it is drawn as those
// characters, in the chip and in the fold, as one text run with no element.
func TestMangoConditionWithMarkupRendersAsText(t *testing.T) {
	t.Parallel()

	const text = `<b>model = opus</b>`
	dom := newPageDOM(t)
	target := mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 36, 0.4, 0.2, MangoEarning)
	target["conditions"] = []any{map[string]any{"text": text, "share": 0.12, "narrows": true}}
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	chip := dom.descend(t, "mango", 0, 1, 1, 4, 0)
	if chip.Text != text+" 12.0% of landings" {
		t.Errorf("the chip draws %q, want the raw condition text and its share", chip.Text)
	}
	if chip.Kids != 0 {
		t.Errorf("the chip holds %d elements, want its text alone", chip.Kids)
	}
	folded := dom.descend(t, "mango", 0, 1, 1, 4, 1, 1, 0, 0)
	if folded.Text != text {
		t.Errorf("the fold draws %q, want the raw condition text", folded.Text)
	}
	if folded.Kids != 0 {
		t.Errorf("the fold's condition holds %d elements, want its text alone", folded.Kids)
	}
}

// A panel that reports one interval bound, or a high bound below its low, has
// not measured a range, so the page draws no interval bar for it.
func TestMangoIntervalBarNeedsBothBounds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		low  any
		high any
	}{
		{"a lone bound", 0.3, nil},
		{"a reversed pair", 0.5, 0.3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dom := newPageDOM(t)
			target := mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 36, 0.4, 0.2, MangoEarning)
			target["ci_low"], target["ci_high"] = tc.low, tc.high
			dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

			if n := dom.descend(t, "mango", 0, 1, 1, 3).Kids; n != 0 {
				t.Errorf("the interval cell holds %d parts, want no bar", n)
			}
		})
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
