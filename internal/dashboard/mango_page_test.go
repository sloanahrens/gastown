package dashboard

import (
	"encoding/json"
	"fmt"
	"math"
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

	lines := mangoTexts(t, dom, "mgnot")
	if len(lines) != 1 {
		t.Fatalf("the panel draws %d not-checkable lines, want one", len(lines))
	}
	for _, want := range []string{"not checkable", "reached_overrun_stop", "12 positives", "too few positives to check"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the not-checkable line %q has no %q", lines[0], want)
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
	if big := mangoTexts(t, dom, "mgbig"); len(big) != 2 || big[1] != "-" {
		t.Errorf("a missing rate reads %q, want a dash", big)
	}
	if strings.Contains(rates.Text, "0.0%") {
		t.Errorf("a missing rate is drawn as %q, want no figure the analysis never gave", rates.Text)
	}
	if vs := mangoTexts(t, dom, "mgvs"); len(vs) != 2 || vs[1] != "vs - across all recent landings" {
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
	thin := mangoElements(t, dom, "jvthin")
	if len(thin) != 1 {
		t.Fatalf("the row draws %d thin chips, want one", len(thin))
	}
	if !strings.Contains(thin[0].Text, "thin") || !strings.Contains(thin[0].Text, "n=36") {
		t.Errorf("the thin chip %q does not name thin and the holdout it rests on", thin[0].Text)
	}
	rates := mangoCell(t, dom, 1, 2)
	for _, want := range []string{"41.7%", "vs 19.8% across all recent landings", "2.1x as likely", "15 of 36 recent landings in the group"} {
		if !strings.Contains(rates.Text, want) {
			t.Errorf("the rate cell %q has no %q", rates.Text, want)
		}
	}

	bar := mangoElements(t, dom, "mgbar")
	if len(bar) != 1 || bar[0].Kids != 4 {
		t.Fatalf("the interval draws %v, want one bar of four parts", bar)
	}
	if got := mangoAttrs(t, dom, "mgbar", "aria-label"); len(got) != 1 || got[0] != "41.7%, interval 27.1% to 57.8%" {
		t.Errorf("the interval bar's label is %q, want the rate and the interval it draws", got)
	}

	chips := mangoElements(t, dom, "mgcondchip")
	if len(chips) != 2 {
		t.Fatalf("the row draws %d narrowing chips, want two", len(chips))
	}
	for i, want := range []string{"model = opus 12.0% of landings", "attempts in q4 18.0% of landings"} {
		if chips[i].Text != want {
			t.Errorf("the narrowing chip %d reads %q, want %q", i, chips[i].Text, want)
		}
	}
	fold := mangoTexts(t, dom, "mgfold")
	if len(fold) != 1 || !strings.Contains(fold[0], "how it was found") {
		t.Fatalf("the fold reads %q, want it named how it was found", fold)
	}
	conds := mangoElements(t, dom, "mgcond")
	if len(conds) != 3 {
		t.Fatalf("the fold holds %d conditions, want all three", len(conds))
	}
	for i, want := range []string{"model = opus", "attempts in q4", "rig = gastown"} {
		if !strings.Contains(conds[i].Text, want) || !strings.Contains(conds[i].Text, "of landings") {
			t.Errorf("the fold's condition %d reads %q, want %q and its share", i, conds[i].Text, want)
		}
	}
	if hasClass(conds[0], "jvstale") || hasClass(conds[1], "jvstale") {
		t.Errorf("a condition that narrows is greyed: %q and %q", conds[0].Class, conds[1].Class)
	}
	if !hasClass(conds[2], "jvstale") {
		t.Errorf("the condition that does not narrow is drawn as %q, want it greyed", conds[2].Class)
	}
	if !strings.Contains(conds[2].Text, "doesn't narrow") {
		t.Errorf("the condition that does not narrow reads %q, want it marked", conds[2].Text)
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
	line := mangoTexts(t, dom, "mgnot")
	if len(line) != 1 {
		t.Fatalf("the panel draws %d not-checkable lines, want one", len(line))
	}
	if chip := mangoTexts(t, dom, "jv-notyet"); len(chip) != 1 || chip[0] != "not checkable" {
		t.Errorf("the line's chip is %q, want the dim not-checkable chip", chip)
	}
	for _, want := range []string{"Beads stopped by the overrun guard", "12 positives", "no positives in the recent landings to check it on, its last positive was 2026-10-19"} {
		if !strings.Contains(line[0], want) {
			t.Errorf("the not-checkable line %q has no %q", line[0], want)
		}
	}
}

// A reason word the page has no sentence for is drawn as the panel's own word:
// a line that dropped it would leave the operator with no reason at all.
func TestMangoNotCheckableKeepsAReasonItCannotName(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	target := mangoPageTarget("reached_overrun_stop", 12, true, "", 0, 0, 0.03, MangoNotYet)
	target["reason"] = "warmup_landings"
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	line := mangoTexts(t, dom, "mgnot")
	if len(line) != 1 {
		t.Fatalf("the panel draws %d not-checkable lines, want one", len(line))
	}
	if !strings.Contains(line[0], "warmup_landings") {
		t.Errorf("the not-checkable line %q drops the panel's reason word", line[0])
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
	if chips := mangoElements(t, dom, "mgcondchip"); len(chips) != 0 {
		t.Errorf("an old panel draws %d condition chips, want none", len(chips))
	}
	if fold := mangoTexts(t, dom, "mgfold"); len(fold) != 2 {
		t.Errorf("the two rows draw %d folds, want one each", len(fold))
	} else if !strings.Contains(fold[0], "model = opus and attempts in q4") {
		t.Errorf("the fold holds %q, want the old best box", fold[0])
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

	chip := mangoElements(t, dom, "mgcondchip")
	if len(chip) != 1 {
		t.Fatalf("the row draws %d narrowing chips, want one", len(chip))
	}
	if chip[0].Text != text+" 12.0% of landings" {
		t.Errorf("the chip draws %q, want the raw condition text and its share", chip[0].Text)
	}
	if chip[0].Kids != 0 {
		t.Errorf("the chip holds %d elements, want its text alone", chip[0].Kids)
	}
	folded := mangoElements(t, dom, "mgcondtext")
	if len(folded) != 1 {
		t.Fatalf("the fold draws %d condition texts, want one", len(folded))
	}
	if folded[0].Text != text {
		t.Errorf("the fold draws %q, want the raw condition text", folded[0].Text)
	}
	if folded[0].Kids != 0 {
		t.Errorf("the fold's condition holds %d elements, want its text alone", folded[0].Kids)
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

			if bars := mangoElements(t, dom, "mgbar"); len(bars) != 0 {
				t.Errorf("the interval draws %d bars, want none", len(bars))
			}
		})
	}
}

// A fresh panel with a weekly series draws the trend under its targets: a dot
// and a percentage at each week, the week and its landings under the dot, one
// marker per regime event with its text, and the caption under the chart.
func TestMangoPanelDrawsTheWeeklyTrend(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
		mangoPageWeek("2026-10-19", 21, 0.30),
		mangoPageWeek("2026-10-26", 18, 0.15),
	}
	state["events"] = []map[string]any{
		mangoPageEvent("2026-10-10", "flash seats replaced by Sonnet seats"),
		mangoPageEvent("2026-10-16", "the landing gate took the exec tax"),
		mangoPageEvent("2026-10-24", "the merge queue landed one branch at a time"),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	if got := mangoAttrs(t, dom, "mgtdot", "cx"); len(got) != 4 {
		t.Errorf("the trend draws %d dots, want one per week (4)", len(got))
	}
	weeks := strings.Join(mangoTexts(t, dom, "mgtwk"), "|")
	for _, want := range []string{"wk of 10-05 (27)", "wk of 10-12 (34)", "wk of 10-19 (21)", "wk of 10-26 (18)"} {
		if !strings.Contains(weeks, want) {
			t.Errorf("the week labels %q have no %q", weeks, want)
		}
	}
	pcts := strings.Join(mangoTexts(t, dom, "mgtpct"), "|")
	for _, want := range []string{"25.0%", "12.5%", "30.0%", "15.0%"} {
		if !strings.Contains(pcts, want) {
			t.Errorf("the point labels %q have no %q", pcts, want)
		}
	}
	if got := mangoAttrs(t, dom, "mgtmark", "x1"); len(got) != 3 {
		t.Errorf("the trend draws %d event markers, want one per event (3)", len(got))
	}
	whats := strings.Join(mangoTexts(t, dom, "mgtwhat"), "|")
	for _, want := range []string{"flash seats replaced by Sonnet seats", "the landing gate took the exec tax", "the merge queue landed one branch at a time"} {
		if !strings.Contains(whats, want) {
			t.Errorf("the event labels %q have no %q", whats, want)
		}
	}
	caps := mangoTexts(t, dom, "mgtcap")
	if len(caps) != 1 || caps[0] != "Landings per week in brackets. Four weeks is too few to say what caused the rise; the dates mark when things changed." {
		t.Errorf("the caption reads %q, want the chart's one caption", caps)
	}
}

// A week's dot is placed by its date and its share: a later week is drawn
// further right, and a larger share is drawn higher, which on the page is a
// smaller y.
func TestMangoTrendPlacesPointsByDateAndShare(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.10),
		mangoPageWeek("2026-10-26", 18, 0.20),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	cx := mangoAttrs(t, dom, "mgtdot", "cx")
	cy := mangoAttrs(t, dom, "mgtdot", "cy")
	if len(cx) != 2 || len(cy) != 2 {
		t.Fatalf("the trend draws %d dots at %v, want two", len(cx), cx)
	}
	x0, x1 := mustFloat(t, cx[0]), mustFloat(t, cx[1])
	y0, y1 := mustFloat(t, cy[0]), mustFloat(t, cy[1])
	if x1 <= x0 {
		t.Errorf("the later week is drawn at x=%v, not right of the earlier week's x=%v", x1, x0)
	}
	if y1 >= y0 {
		t.Errorf("the 20%% week is drawn at y=%v, not above the 10%% week's y=%v", y1, y0)
	}
}

// An event outside the trend's span has no x on the chart, so it draws no
// marker and its text is not drawn.
func TestMangoTrendLeavesOutEventsOutsideItsSpan(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
	}
	state["events"] = []map[string]any{
		mangoPageEvent("2026-09-30", "before the first week"),
		mangoPageEvent("2026-10-08", "inside the trend"),
		mangoPageEvent("2026-10-20", "after the last week and its seven days"),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	if got := mangoAttrs(t, dom, "mgtmark", "x1"); len(got) != 1 {
		t.Errorf("the trend draws %d markers, want only the event inside its span", len(got))
	}
	whats := mangoTexts(t, dom, "mgtwhat")
	if len(whats) != 1 || whats[0] != "inside the trend" {
		t.Errorf("the event labels read %q, want only the event inside the span", whats)
	}
}

// An event label is drawn to the right of its marker, and left of it when the
// text would run off the chart. Two events whose labels would touch are drawn
// at different heights rather than overprinting.
func TestMangoEventLabelsTakeDistinctHeightsAndFlipAtTheRightEdge(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
	}
	state["events"] = []map[string]any{
		mangoPageEvent("2026-10-07", "an early change with a label that fits on the right"),
		mangoPageEvent("2026-10-17", "a late change with a label too long for the right edge"),
		mangoPageEvent("2026-10-18", "another late change a day after the first one"),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	anchors := mangoAttrs(t, dom, "mgtwhat", "text-anchor")
	want := []string{"start", "end", "end"}
	if strings.Join(anchors, ",") != strings.Join(want, ",") {
		t.Errorf("the event labels are anchored %v, want %v", anchors, want)
	}
	y := mangoAttrs(t, dom, "mgtwhat", "y")
	if len(y) != 3 {
		t.Fatalf("the trend draws %d event labels, want three", len(y))
	}
	if y[1] == y[2] {
		t.Errorf("the two late events are drawn at the same height y=%s, want distinct heights", y[1])
	}
}

// mangoLabelBox is one event label as its text covers the chart: the span the
// drawn characters occupy and the height they sit on.
type mangoLabelBox struct {
	Text  string
	Y, Lo float64
	Hi    float64
}

// mangoMonospaceEm is the advance of the widest face in the page's monospace
// stack, as a fraction of the font size: Menlo's glyphs are 1233/2048 em
// (0.602), SF Mono's 0.6 and Consolas's 0.55. It is the font's own figure and
// not the page's, which allots 5.6px a character at font-size 9; a test that
// measured with the page's figure could not see an estimate that under-measures
// the drawn text (gt-ke4jw).
const mangoMonospaceEm = 0.602

// mangoLabelBoxes reads the event labels back as the boxes their text covers:
// each label's own font size, as the page drew it, times its rune count at the
// widest advance the stack names.
func mangoLabelBoxes(t *testing.T, dom *pageDOM) []mangoLabelBox {
	t.Helper()

	xs := mangoAttrs(t, dom, "mgtwhat", "x")
	ys := mangoAttrs(t, dom, "mgtwhat", "y")
	anchors := mangoAttrs(t, dom, "mgtwhat", "text-anchor")
	sizes := mangoAttrs(t, dom, "mgtwhat", "font-size")
	texts := mangoTexts(t, dom, "mgtwhat")
	if len(xs) != len(ys) || len(xs) != len(anchors) || len(xs) != len(sizes) || len(xs) != len(texts) {
		t.Fatalf("the event labels read %d x, %d y, %d anchors, %d font sizes and %d texts, want one of each", len(xs), len(ys), len(anchors), len(sizes), len(texts))
	}
	boxes := make([]mangoLabelBox, 0, len(texts))
	for i := range texts {
		if sizes[i] == "" {
			t.Fatalf("the label %q is drawn with no font size, so the box its text covers cannot be measured", texts[i])
		}
		x, y := mustFloat(t, xs[i]), mustFloat(t, ys[i])
		w := float64(len([]rune(texts[i]))) * mangoMonospaceEm * mustFloat(t, sizes[i])
		b := mangoLabelBox{Text: texts[i], Y: y, Lo: x, Hi: x + w}
		if anchors[i] == "end" {
			b.Lo, b.Hi = x-w, x
		}
		boxes = append(boxes, b)
	}
	return boxes
}

// mangoChartWidth reads the width the chart clips at from the viewBox the page
// draws it with, so the bounds check holds a label to the box drawn rather than
// to a width copied out of the layout (gt-ke4jw).
func mangoChartWidth(t *testing.T, dom *pageDOM) float64 {
	t.Helper()

	views := mangoAttrs(t, dom, "mgchart", "viewBox")
	if len(views) != 1 {
		t.Fatalf("the trend draws %d charts, want one", len(views))
	}
	fields := strings.Fields(views[0])
	if len(fields) != 4 {
		t.Fatalf("the chart's viewBox reads %q, want four numbers", views[0])
	}
	return mustFloat(t, fields[2])
}

// Event labels never cover each other's text and never leave the chart's box,
// at any page width: the page draws the chart as a viewBox it scales as one
// piece, so a layout that parts its labels in chart units parts them on screen
// too, and one that overlaps covers text at 1760px as at any other width
// (gt-ejvpk).
func TestMangoTrendEventLabelsKeepTheirTextApartAndInsideTheChart(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-09-28", 31, 0.35),
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
		mangoPageWeek("2026-10-19", 29, 0.31),
	}
	// Three events a few days apart, the cluster the dashboard drew on top of
	// each other, and one before and one after them, where a label runs into
	// the chart's edge.
	state["events"] = []map[string]any{
		mangoPageEvent("2026-09-28", "the first week of the span has a change of its own to name"),
		mangoPageEvent("2026-10-02", "from gate threshold 0.75 on gastown the queue waited a day"),
		mangoPageEvent("2026-10-06", "from gate threshold 0.6 to 0.7 on gastown"),
		mangoPageEvent("2026-10-07", "haiku seats become the dominant model on the rig"),
		mangoPageEvent("2026-10-09", "the queue opened early and every branch waited a full day"),
		mangoPageEvent("2026-10-25", "the last day of the span gets a change of its own too"),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	boxes := mangoLabelBoxes(t, dom)
	if len(boxes) != 6 {
		t.Fatalf("the trend draws %d event labels, want one per event (6)", len(boxes))
	}
	chartW := mangoChartWidth(t, dom)
	for _, b := range boxes {
		if b.Lo < 0 || b.Hi > chartW {
			t.Errorf("the label %q covers x %.0f..%.0f, want it inside the chart's 0..%.0f", b.Text, b.Lo, b.Hi, chartW)
		}
	}
	for i := 0; i < len(boxes); i++ {
		for j := i + 1; j < len(boxes); j++ {
			a, b := boxes[i], boxes[j]
			if a.Y != b.Y || a.Hi <= b.Lo || b.Hi <= a.Lo {
				continue
			}
			t.Errorf("the labels %q and %q share the row at y=%.0f and cover the same text at x %.0f..%.0f", a.Text, b.Text, a.Y, math.Max(a.Lo, b.Lo), math.Min(a.Hi, b.Hi))
		}
	}
}

// A row already taken is not reused: a fourth colliding label adds a row and
// the page lengthens the chart so the row has height above the plot, where the
// markers stay on the dates they mark (gt-ejvpk).
func TestMangoTrendAddsARowAndLengthensTheChartForAFourthLabel(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
	}
	dates := []string{"2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10"}
	events := make([]map[string]any, 0, len(dates))
	var want []string
	for _, date := range dates {
		what := "a change on " + date + " that no other label has room beside"
		events = append(events, mangoPageEvent(date, what))
		want = append(want, what)
	}
	state["events"] = events
	dom.call(t, "renderMango", map[string]any{"mango": state})

	y := mangoAttrs(t, dom, "mgtwhat", "y")
	if len(y) != len(want) {
		t.Fatalf("the trend draws %d event labels, want one per event (%d)", len(y), len(want))
	}
	if got := strings.Join(mangoTexts(t, dom, "mgtwhat"), "|"); got != strings.Join(want, "|") {
		t.Fatalf("the event labels read %q, want every label drawn in date order", got)
	}
	seen := map[string]bool{}
	for _, h := range y {
		if seen[h] {
			t.Errorf("two colliding labels share the row at y=%s, want each on a row of its own", h)
		}
		seen[h] = true
	}
	marks := mangoAttrs(t, dom, "mgtmark", "y1")
	if len(marks) != len(want) {
		t.Fatalf("the trend draws %d event markers, want one per event (%d)", len(marks), len(want))
	}
	// The marker runs from the plot's top to its bottom; the labels sit above it.
	top := mustFloat(t, marks[0])
	for _, h := range y {
		if got := mustFloat(t, h); got >= top {
			t.Errorf("the label at y=%.0f starts below the plot top at y=%.0f, want every row above it", got, top)
		}
	}
	// Two weeks span fourteen days, so the x axis is not moved by the rows the
	// labels take: each marker sits on its own day of that span.
	first := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	xs := mangoAttrs(t, dom, "mgtmark", "x1")
	for i, date := range []string{"2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10"} {
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			t.Fatalf("reading the event date %q: %v", date, err)
		}
		wantX := 44 + float64(d.Sub(first))/(float64(14*24*time.Hour))*(656-44)
		if got := mustFloat(t, xs[i]); math.Abs(got-wantX) > 0.05 {
			t.Errorf("the marker for %s is at x=%.1f, want x=%.1f", date, got, wantX)
		}
	}
	boxes := mangoAttrs(t, dom, "mgchart", "viewBox")
	if len(boxes) != 1 || !strings.HasPrefix(boxes[0], "0 0 700 ") {
		t.Fatalf("the chart's viewBox reads %q, want one at width 700", boxes)
	}
	if h := mustFloat(t, strings.TrimPrefix(boxes[0], "0 0 700 ")); h <= 170 {
		t.Errorf("the chart is %.0f tall with four label rows, want it lengthened past its base 170", h)
	}
}

// Three labels that all overlap take three height bands: each one's own extent
// decides its band, so a stack of them never overprints two lines of text.
func TestMangoTrendStacksThreeCollidingLabels(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
	}
	state["events"] = []map[string]any{
		mangoPageEvent("2026-10-16", "the queue opened early and every branch waited a full day"),
		mangoPageEvent("2026-10-17", "the gate closed early and every branch waited two days"),
		mangoPageEvent("2026-10-18", "the gate reopened late and every branch ran one at a time"),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	y := mangoAttrs(t, dom, "mgtwhat", "y")
	if len(y) != 3 {
		t.Fatalf("the trend draws %d event labels, want three", len(y))
	}
	for i := 0; i < len(y); i++ {
		for j := i + 1; j < len(y); j++ {
			if y[i] == y[j] {
				t.Errorf("the colliding labels at y=%s and y=%s share a height, want each on its own band", y[i], y[j])
			}
		}
	}
}

// Fewer than two weeks is not a trend: the block says so and draws no chart. A
// panel with no weekly key at all is an older panel, which gets no trend block
// and still draws its table.
func TestMangoTrendWithoutTwoWeeksSaysSoAndWithoutTheKeyDrawsNoBlock(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		weekly []map[string]any
	}{
		{"one week", []map[string]any{mangoPageWeek("2026-10-05", 27, 0.25)}},
		{"no weeks", []map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			state := mangoPageState(MangoFresh, "fresh",
				mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
			state["weekly"] = tc.weekly
			dom.call(t, "renderMango", map[string]any{"mango": state})

			heads := mangoTexts(t, dom, "mgthead")
			if len(heads) != 1 || heads[0] != "not enough weeks yet" {
				t.Errorf("the trend heading reads %q, want the not-enough-weeks line", heads)
			}
			if got := mangoAttrs(t, dom, "mgchart", "viewBox"); len(got) != 0 {
				t.Errorf("the panel draws a chart from %q, want none with fewer than two weeks", got)
			}
			for _, class := range []string{"mgtline", "mgtdot", "mgtmark"} {
				if got := mangoAttrs(t, dom, class, "class"); len(got) != 0 {
					t.Errorf("the panel draws %d %q marks with no chart, want none", len(got), class)
				}
			}
			if got := mangoTexts(t, dom, "mgtcap"); len(got) != 0 {
				t.Errorf("the panel draws the caption %q with no chart, want none", got)
			}
		})
	}

	t.Run("no weekly key", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh",
			mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))})

		if got := mangoTexts(t, dom, "mgt"); len(got) != 0 {
			t.Errorf("a panel with no weekly key draws a trend block %q, want none", got)
		}
		if got := mangoCell(t, dom, 1, 0).Text; got != "rejected_before_landing" {
			t.Errorf("the target row reads %q, want the table to still draw", got)
		}
	})
}

// An event whose what carries markup is drawn as that text, not as elements:
// the page builds every label with textContent.
func TestMangoEventWithMarkupRendersAsText(t *testing.T) {
	t.Parallel()

	const text = "<b>flash</b> seats replaced"
	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
	}
	state["events"] = []map[string]any{mangoPageEvent("2026-10-08", text)}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	whats := mangoTexts(t, dom, "mgtwhat")
	if len(whats) != 1 || whats[0] != text {
		t.Errorf("the event label reads %q, want the raw text %q", whats, text)
	}
	if kids := mangoKids(t, dom, "mgtwhat"); len(kids) != 1 || kids[0] != 0 {
		t.Errorf("the event label holds %v children, want its text alone", kids)
	}
}

// An event's what longer than the chart's label width is cut at a word boundary
// and ends in an ellipsis, so the text stays inside the viewBox instead of
// running past its edge, where the SVG would clip it without a sign.
func TestMangoTrendTruncatesALongEventLabel(t *testing.T) {
	t.Parallel()

	const long = "the merge queue landed one branch at a time for the whole of the season while the gate stayed closed"
	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
	}
	state["events"] = []map[string]any{mangoPageEvent("2026-10-08", long)}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	whats := mangoTexts(t, dom, "mgtwhat")
	if len(whats) != 1 {
		t.Fatalf("the trend draws %d event labels, want one", len(whats))
	}
	got := whats[0]
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("the long event label reads %q, want it cut with an ellipsis", got)
	}
	kept := strings.TrimSuffix(got, "…")
	if !strings.HasPrefix(long, kept) {
		t.Errorf("the label reads %q, which is not the start of the what %q", got, long)
	}
	if len(kept) >= len(long) {
		t.Errorf("the label reads %q, want the long what cut", got)
	}
	if long[len(kept)] != ' ' {
		t.Errorf("the label cuts the what mid-word at %q, want a word boundary", kept)
	}
	if len([]rune(got)) > 60 {
		t.Errorf("the label is %d characters, want at most the chart's 60", len([]rune(got)))
	}
}

// The caption names the number of weeks the chart holds, so a series that is
// not four weeks does not read as four.
func TestMangoTrendCaptionNamesTheWeeksItHolds(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-09-14", 20, 0.10),
		mangoPageWeek("2026-09-21", 24, 0.12),
		mangoPageWeek("2026-09-28", 19, 0.15),
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
		mangoPageWeek("2026-10-19", 21, 0.30),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	caps := mangoTexts(t, dom, "mgtcap")
	want := "Landings per week in brackets. Six weeks is too few to say what caused the rise; the dates mark when things changed."
	if len(caps) != 1 || caps[0] != want {
		t.Errorf("the caption reads %q, want %q", caps, want)
	}
}

// A week whose share is not a number is not a point: the page drops it rather
// than drawing a mark at NaN, which the SVG would not draw at all.
func TestMangoTrendDropsAWeekWithNoShare(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoFresh, "fresh",
		mangoPageTarget("rejected_before_landing", 48, false, "model = opus", 96, 0.42, 0.25, MangoEarning))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		{"week": "2026-10-12", "landings": 34},
		mangoPageWeek("2026-10-19", 21, 0.30),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	cx, cy := mangoAttrs(t, dom, "mgtdot", "cx"), mangoAttrs(t, dom, "mgtdot", "cy")
	if len(cx) != 2 || len(cy) != 2 {
		t.Fatalf("the trend draws %d dots at %v, want the two weeks with a share", len(cx), cx)
	}
	for i := range cx {
		if v := mustFloat(t, cx[i]); math.IsNaN(v) {
			t.Errorf("a dot is drawn at x=%q, want a number", cx[i])
		}
		if v := mustFloat(t, cy[i]); math.IsNaN(v) {
			t.Errorf("a dot is drawn at y=%q, want a number", cy[i])
		}
	}
	if weeks := strings.Join(mangoTexts(t, dom, "mgtwk"), "|"); strings.Contains(weeks, "10-12") {
		t.Errorf("the week labels %q have the week with no share, want it dropped", weeks)
	}
}

// A stale panel greys the trend with the rest of it: the trend is drawn from
// the same last analysis the rows say is old.
func TestMangoStalePanelGreysTheTrend(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	state := mangoPageState(MangoStale, "stale",
		mangoPageTarget("manual_rescue", 37, false, "attempts in q3..q4", 54, 0.11, 0.09, MangoNoSignal))
	state["weekly"] = []map[string]any{
		mangoPageWeek("2026-10-05", 27, 0.25),
		mangoPageWeek("2026-10-12", 34, 0.125),
	}
	dom.call(t, "renderMango", map[string]any{"mango": state})

	classes := mangoClassValues(t, dom, "mgt", "c.className")
	if len(classes) != 1 || !strings.Contains(classes[0], "jvstale") {
		t.Errorf("a stale panel's trend block is drawn as %q, want the jvstale class", classes)
	}
}

// TestMangoRowFitsThePanelAtTheWidthItIsReadAt lays the Mango table out the way
// the page declares where its six columns are drawn — a panel at 800px, above
// the width the stacked layout takes over — and measures the text each body cell
// draws against the column it lands in: the table fills its panel and lays out
// fixed, so a column takes its declared share and no cell's text can size it,
// and every body cell breaks its text, so no unbroken run is wider than its
// column and nothing crosses the panel's right edge. The best box takes the
// widest column because it carries the row's longest prose, so a condition
// wraps as words rather than a character a line. The panel's real width, about
// 620px, draws the stacked layout, which TestMangoStackedRowFitsThePanel
// measures (gt-6q7zj, gt-c6ge9).
func TestMangoRowFitsThePanelAtTheWidthItIsReadAt(t *testing.T) {
	t.Parallel()

	table := cssDecls(t, "mgtable")
	if got := table["table-layout"]; got != "fixed" {
		t.Errorf("the Mango table lays out %q, want fixed so a cell's text sizes no column", got)
	}
	if got := table["width"]; got != "100%" {
		t.Errorf("the Mango table takes %q of the panel, want 100%% so its shares are the whole row", got)
	}
	requireWideTable(t)

	dom := newPageDOM(t)
	target := mangoPageTarget("rejected_before_landing", 48, false, "model = opus and attempts in q4", 96, 0.42, 0.25, MangoEarning)
	target["label"] = "Landings rejected before they land"
	target["conditions"] = []any{
		map[string]any{"text": "model = opus", "share": 0.12, "narrows": true},
		map[string]any{"text": "attempts in q4", "share": 0.18, "narrows": true},
		map[string]any{"text": "rig = gastown", "share": 0.71, "narrows": false},
	}
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	widths, boxAt := mangoWideColumnWidths(t, dom)

	// The box draws the panel's longest prose — a condition runs to 240
	// characters where a label stops at 60 (mango.go) — so no other column may
	// take the room it needs to wrap in.
	for i, w := range widths {
		if w > widths[boxAt]+0.5 {
			t.Errorf("the %s column takes %gpx of the row, more than the best box's %gpx, though the box carries its longest prose", mangoColumnName(t, dom, i), w, widths[boxAt])
		}
	}

	// Every cell of every row keeps its text inside its column at the page's
	// font. A run the cell cannot break, wider than the space left by its
	// padding, is text past the panel's right edge.
	pad := boxSide(t, topRule(t, ".mgtable td"), "padding", "right")
	lastPad := boxSide(t, topRule(t, ".mgtable td:last-child"), "padding", "right")
	base := mangoText{whiteSpace: "normal", font: cssPxValue(t, declOf(t, topRule(t, "html,body"), "font"))}
	for r, row := range mangoRows(t, dom) {
		if len(row) != len(widths) {
			t.Fatalf("body row %d draws %d cells, want the header's %d columns", r, len(row), len(widths))
		}
		for c, cell := range row {
			avail := widths[c] - pad
			if c == len(row)-1 {
				avail = widths[c] - lastPad
			}
			for _, run := range cell.runs(t, base) {
				text := float64(len([]rune(run.Text))) * run.Font * monospaceAdvance
				if text > avail+0.5 {
					t.Errorf("body row %d column %d draws the unbroken %q in %gpx, wider than the %gpx its cell leaves: the text would cross the panel's right edge", r, c, run.Text, text, avail)
				}
			}
		}
	}
}

// TestMangoRateReadsInTwoLinesAndItsRowStaysShort draws the row where the
// six-column table is drawn — a panel at 800px, its font stack at the widest
// face it names — and lays the drawn cells out the way the stylesheet says: the
// baseline line takes at most two lines, and no cell draws taller than the
// 120px a reader takes in at a glance. The fixture is a full modern row: a
// 34-character label, a rate with its group count, an interval, two narrow
// condition chips and a 33-character best box. The unit tier renders nothing,
// so the layout is measured from the declarations the stylesheet sets against
// the text the row draws. The panel's real width draws the stacked layout,
// whose row TestMangoStackedRowFitsThePanel measures (gt-g4vif, gt-c6ge9).
func TestMangoRateReadsInTwoLinesAndItsRowStaysShort(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	const label = "Landings rejected before they land"
	target := mangoPageTarget("rejected_before_landing", 48, false, "model = opus and attempts in q4", 96, 0.42, 0.243, MangoEarning)
	target["label"] = label
	target["holdout_positives"] = 44
	target["ci_low"] = 0.31
	target["ci_high"] = 0.53
	target["conditions"] = []any{
		map[string]any{"text": "model = opus", "share": 0.12, "narrows": true},
		map[string]any{"text": "attempts in q4", "share": 0.18, "narrows": true},
		map[string]any{"text": "rig = gastown", "share": 0.71, "narrows": false},
	}
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	requireWideTable(t)
	widths, _ := mangoWideColumnWidths(t, dom)
	row := mangoRowNamed(t, mangoRows(t, dom), label)
	if len(row) != len(widths) {
		t.Fatalf("the target row draws %d cells, want the header's %d columns", len(row), len(widths))
	}

	rateAt := -1
	for i := range widths {
		if mangoColumnName(t, dom, i) == "rate" {
			rateAt = i
		}
	}
	if rateAt < 0 {
		t.Fatal("the Mango header names no rate column")
	}

	// The line the bead measures: the baseline the rate is read against.
	const baseline = "vs 24.3% across all recent landings"
	base := mangoMeasureBase(t)
	rateWidth := widths[rateAt] - mangoCellRightPadding(t)
	vs := -1
	for i, kid := range row[rateAt].Kids {
		if mangoNamesClass(kid.Classes, "mgvs") {
			vs = i
		}
	}
	if vs < 0 {
		t.Fatalf("the rate cell draws no baseline line, want %q", baseline)
	}
	if got := strings.TrimSpace(row[rateAt].Kids[vs].All); got != baseline {
		t.Fatalf("the rate cell's baseline line reads %q, want %q", got, baseline)
	}
	if msg := mangoBaselineProblem(baseline, mangoLineCount(t, row[rateAt].Kids[vs], base, rateWidth), rateWidth); msg != "" {
		t.Error(msg)
	}

	// A row is as tall as its tallest cell, its cells' padding included. 120px
	// is one row a reader takes in at a glance rather than a column of words.
	const limit = 120
	lastPad := mangoLastCellRightPadding(t)
	pad := mangoCellVerticalPadding(t)
	for c, cell := range row {
		avail := widths[c] - mangoCellRightPadding(t)
		if c == len(row)-1 {
			avail = widths[c] - lastPad
		}
		h := mangoCellHeight(t, cell, base, avail) + pad + mangoRowBorder(t)
		if msg := mangoRowHeightProblem(mangoColumnName(t, dom, c), h, limit); msg != "" {
			t.Error(msg)
		}
	}
}

// TestMangoStackedRowFitsThePanel draws the full modern row at the panel's real
// width, where the page stacks a target row into three lines of two equal
// tracks, and measures what it draws: the stacked layout is the one drawn at
// that width, every unbroken run fits the track its cell lands in, and the
// row's three lines with their gaps, padding and rule stay inside the row's
// bound. The fixture is the fullest row the writer produces: a 34-character
// label, a rate with its group count, an interval, two narrow condition chips
// and a 33-character best box (gt-c6ge9).
func TestMangoStackedRowFitsThePanel(t *testing.T) {
	t.Parallel()

	panel := mangoPanelReadWidth(t)
	if got := mangoStackWidth(t); panel > got {
		t.Fatalf("the Mango table stacks only up to %gpx, so the %gpx panel draws six columns rather than the stacked row this test measures", got, panel)
	}
	cell := mangoStackedCellWidth(t)

	dom := newPageDOM(t)
	const label = "Landings rejected before they land"
	target := mangoPageTarget("rejected_before_landing", 48, false, "model = opus and attempts in q4", 96, 0.42, 0.243, MangoEarning)
	target["label"] = label
	target["holdout_positives"] = 44
	target["ci_low"] = 0.31
	target["ci_high"] = 0.53
	target["conditions"] = []any{
		map[string]any{"text": "model = opus", "share": 0.12, "narrows": true},
		map[string]any{"text": "attempts in q4", "share": 0.18, "narrows": true},
		map[string]any{"text": "rig = gastown", "share": 0.71, "narrows": false},
	}
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	row := mangoRowNamed(t, mangoRows(t, dom), label)

	// Every unbroken run keeps inside the track its cell draws in at the page's
	// font; a run wider than the track is text past the panel's right edge.
	base := mangoText{whiteSpace: "normal", font: cssPxValue(t, declOf(t, topRule(t, "html,body"), "font"))}
	for c, node := range row {
		for _, run := range node.runs(t, base) {
			text := float64(len([]rune(run.Text))) * run.Font * monospaceAdvance
			if text > cell+0.5 {
				t.Errorf("the stacked %s cell draws the unbroken %q in %gpx, wider than the %gpx its track leaves: the text would cross the panel's right edge", mangoColumnName(t, dom, c), run.Text, text, cell)
			}
		}
	}

	// The bound is the stacked row's own lines and the space around them: line 1
	// draws 20px, line 2 (the rate cell's number, baseline, lift and group count)
	// 63.6px and line 3 48px, so the lines sum to 131.6px; two row gaps add 4px,
	// the row's padding 16px and its rule 1px, and the row draws 152.6px. The
	// bound is 170px, which holds that sum with room for a face wider than the
	// one measured (gt-c6ge9).
	const limit = 170
	h := mangoStackedRowHeight(t, row, mangoMeasureBase(t), cell)
	if msg := mangoRowHeightProblem("stacked target row", h, limit); msg != "" {
		t.Error(msg)
	}
}

// mangoMeasure is the text style a Mango cell draws with: the properties that
// decide where a line breaks and how tall its line box is. The page writes a
// unitless line-height, which inherits as a factor of each element's own font
// size; a chip writes its line box in pixels instead.
type mangoMeasure struct {
	wrap     string
	font     float64
	lhFactor float64
	lhPx     float64
}

// apply returns the style the node's classes give it, outermost class first, as
// the cascade serves an inner rule over the outer one it was written against.
func (m mangoMeasure) apply(t *testing.T, classes []string) mangoMeasure {
	t.Helper()

	for _, class := range classes {
		decls := cssDecls(t, class)
		if v, ok := decls["font-size"]; ok {
			m.font = cssPxValue(t, v)
		}
		if v, ok := decls["overflow-wrap"]; ok {
			m.wrap = v
		}
		if v, ok := decls["line-height"]; ok {
			if strings.HasSuffix(v, "px") {
				m.lhPx, m.lhFactor = cssPxValue(t, v), 0
			} else {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					t.Fatalf("line-height %q is neither a factor nor a pixel height", v)
				}
				m.lhFactor, m.lhPx = f, 0
			}
		}
	}
	return m
}

// lineBox is the height one of the style's lines takes.
func (m mangoMeasure) lineBox() float64 {
	if m.lhPx > 0 {
		return m.lhPx
	}
	return m.font * m.lhFactor
}

// mangoMeasureBase is the style every cell starts from: the page sets the font
// and its leading on html and body.
func mangoMeasureBase(t *testing.T) mangoMeasure {
	t.Helper()

	size, lead, ok := strings.Cut(declOf(t, topRule(t, "html,body"), "font"), "/")
	if !ok {
		t.Fatal("html,body's font states no line-height")
	}
	lead, _, _ = strings.Cut(lead, " ")
	factor, err := strconv.ParseFloat(strings.TrimSpace(lead), 64)
	if err != nil {
		t.Fatalf("html,body's line-height %q is not a factor: %v", lead, err)
	}
	return mangoMeasure{wrap: "normal", font: cssPxValue(t, size), lhFactor: factor}
}

// mangoLineCount is the number of lines text draws in a box of the given width:
// a word starts the next line when it would cross the edge, and a word wider
// than a whole line breaks inside it only where the page lets a line end at any
// character.
func mangoLineCount(t *testing.T, n mangoNode, base mangoMeasure, width float64) int {
	t.Helper()

	return mangoLines(strings.TrimSpace(n.All), base.apply(t, n.Classes), width)
}

// mangoLines is the number of lines the style's text draws in width pixels.
func mangoLines(text string, m mangoMeasure, width float64) int {
	if strings.TrimSpace(text) == "" {
		return 1
	}
	if width <= 0 {
		// A box with no room draws no line the page can read; counting it as one
		// would pass a column that has collapsed.
		return mangoUnfitLines
	}
	adv := m.font * monospaceAdvance
	lines, used := 1, 0.0
	for _, word := range strings.Fields(text) {
		w := float64(len([]rune(word))) * adv
		if used > 0 && used+adv+w > width {
			lines++
			used = 0
		}
		switch {
		case w <= width:
			if used > 0 {
				used += adv
			}
			used += w
		case m.wrap == "anywhere" || m.wrap == "break-word":
			// The word fills this line and as many more as it needs. A break-word
			// word breaks only when it is wider than a line, so it takes the same
			// lines here as an anywhere word does once it cannot stand whole.
			left := w - (width - used)
			more := int(math.Ceil(left / width))
			lines += more
			used = left - float64(more-1)*width
		default:
			// An unbreakable word stands as one line, past the edge.
			used = w
		}
	}
	return lines
}

// mangoCellHeight is the height one cell's content draws in a column of the
// given width, in pixels: a block child stacks under the one before it, a
// paragraph's words wrap inside the box its cell leaves, a chip wraps inside
// its own border and padding, and a fold the reader has not opened draws only
// its summary. Chips are inline-blocks that a browser could set two to a line;
// this model gives each its own, so it measures no cell shorter than it draws.
func mangoCellHeight(t *testing.T, n mangoNode, base mangoMeasure, width float64) float64 {
	t.Helper()

	m := base.apply(t, n.Classes)
	switch {
	case mangoNamesClass(n.Classes, "mgbar"):
		decls := cssDecls(t, "mgbar")
		return cssPxValue(t, decls["height"]) + mangoInset(t, "mgbar", "margin", "top")
	case mangoNamesClass(n.Classes, "mgfold") && !n.Open && len(n.Kids) > 0:
		// A fold the reader has not opened draws only its summary.
		return mangoInset(t, "mgfold", "margin", "top") + mangoCellHeight(t, n.Kids[0], base, width)
	case mangoNamesClass(n.Classes, "mgfold"):
		top := mangoInset(t, "mgfold", "margin", "top")
		h := 0.0
		for _, kid := range n.Kids {
			h += mangoCellHeight(t, kid, base, width)
		}
		return top + h
	case mangoNamesClass(n.Classes, "jvchip"):
		inner := width - mangoInset(t, "jvchip", "padding", "left") - mangoInset(t, "jvchip", "padding", "right") - 2*mangoChipBorder(t)
		lines := mangoLines(strings.TrimSpace(n.All), m, inner)
		return float64(lines)*m.lineBox() + 2*mangoChipBorder(t) + mangoInset(t, "jvchip", "margin", "top") + mangoInset(t, "jvchip", "margin", "bottom")
	case len(n.Kids) == 0:
		text := strings.TrimSpace(n.Text)
		if text == "" {
			return 0
		}
		return float64(mangoLines(text, m, width)) * m.lineBox()
	}
	if mangoInlineChildren(n.Kids) {
		return mangoParagraphHeight(t, n, base, width)
	}
	total := 0.0
	for _, kid := range n.Kids {
		total += mangoCellHeight(t, kid, base, width)
	}
	return total
}

// mangoParagraphHeight is the height a node draws when everything inside it is
// inline: its words share the cell's lines, and each word takes the line box of
// the font it is drawn in. Every line is measured at the tallest font in the
// paragraph, so the sum is no shorter than the browser's.
func mangoParagraphHeight(t *testing.T, n mangoNode, base mangoMeasure, width float64) float64 {
	t.Helper()

	lines, box := 1, 0.0
	used := 0.0
	var walk func(node mangoNode)
	walk = func(node mangoNode) {
		m := base.apply(t, node.Classes)
		if len(node.Kids) == 0 {
			if box < m.lineBox() {
				box = m.lineBox()
			}
			adv := m.font * monospaceAdvance
			for _, word := range strings.Fields(node.Text) {
				w := float64(len([]rune(word))) * adv
				if used > 0 && used+adv+w > width {
					lines++
					used = 0
				}
				if used > 0 {
					used += adv
				}
				used += w
			}
			return
		}
		for _, kid := range node.Kids {
			walk(kid)
		}
	}
	walk(n)
	return float64(lines) * box
}

// mangoInlineChildren reports whether every child draws in the parent's inline
// flow rather than starting a box of its own. A chip is an inline-block: it
// flows with the text but carries its own border and padding.
func mangoInlineChildren(kids []mangoNode) bool {
	for _, kid := range kids {
		switch kid.Tag {
		case "SPAN", "BUTTON", "A", "B", "I", "EM", "STRONG", "CODE":
			if mangoNamesClass(kid.Classes, "jvchip") {
				return false
			}
		default:
			return false
		}
	}
	return len(kids) > 0
}

// mangoNamesClass reports whether the class chain names class as a whole class.
func mangoNamesClass(classes []string, class string) bool {
	for _, name := range classes {
		if name == class {
			return true
		}
	}
	return false
}

// mangoInset is one side of a class's padding, border or margin, read the way
// the cascade applies it: a longhand declaration wins over the shorthand.
func mangoInset(t *testing.T, class, prop, side string) float64 {
	t.Helper()

	decls := cssDecls(t, class)
	if v, ok := decls[prop+"-"+side]; ok {
		return cssPxValue(t, v)
	}
	v, ok := decls[prop]
	if !ok {
		return 0
	}
	return boxSide(t, prop+":"+v, prop, side)
}

// mangoChipBorder is the width of the one border a chip draws on each side.
func mangoChipBorder(t *testing.T) float64 {
	t.Helper()

	return cssPxValue(t, cssDecls(t, "jvchip")["border"])
}

// mangoCellRightPadding is the space a column keeps between its cell's text and
// the next column.
func mangoCellRightPadding(t *testing.T) float64 {
	t.Helper()

	return boxSide(t, topRule(t, ".mgtable td"), "padding", "right")
}

// mangoLastCellRightPadding is the last column's, which drops the gutter so the
// best box reaches the panel's edge.
func mangoLastCellRightPadding(t *testing.T) float64 {
	t.Helper()

	return boxSide(t, topRule(t, ".mgtable td:last-child"), "padding", "right")
}

// mangoCellVerticalPadding is the space a cell keeps above and below its text.
func mangoCellVerticalPadding(t *testing.T) float64 {
	t.Helper()

	decls := topRule(t, ".mgtable td")
	return boxSide(t, decls, "padding", "top") + boxSide(t, decls, "padding", "bottom")
}

// mangoRowBorder is the rule the table draws above a body row after the first.
func mangoRowBorder(t *testing.T) float64 {
	t.Helper()

	return boxSide(t, topRule(t, ".mgtable td"), "border", "top")
}

// mangoColumnWidths reads the Mango table's columns in pixels for a table of
// the given width: each header cell names a class, and that class declares its
// share of the row. The shares must add up to the row, or a column takes what
// the others leave and the box's room is a guess. It returns the widths and the
// best box's index.
func mangoColumnWidths(t *testing.T, dom *pageDOM, tablePx float64) ([]float64, int) {
	t.Helper()

	const expr = `JSON.stringify((function () {
  var table = __byId["mango"].children.find(function (c) { return String(c.className).split(" ").indexOf("mgtable") >= 0; });
  return table.children[0].children[0].children.map(function (th) { return {text: th.textContent, class: th.className}; });
})())`
	v, err := dom.vm.RunString(expr)
	if err != nil {
		t.Fatalf("reading the Mango header: %v", err)
	}
	var head []drawn
	if err := json.Unmarshal([]byte(v.String()), &head); err != nil {
		t.Fatalf("reading the Mango header: %v", err)
	}
	if len(head) == 0 {
		t.Fatal("the Mango table draws no header")
	}

	widths := make([]float64, len(head))
	boxAt, total := -1, 0.0
	for i, th := range head {
		share := 0.0
		for _, class := range strings.Fields(th.Class) {
			if w, ok := cssDecls(t, class)["width"]; ok {
				pct, err := strconv.ParseFloat(strings.TrimSuffix(w, "%"), 64)
				if err != nil || !strings.HasSuffix(w, "%") {
					t.Fatalf("the Mango column %q takes width %q, want a share of the row", th.Text, w)
				}
				share = pct
			}
		}
		if share == 0 {
			t.Fatalf("the Mango header's column %q is drawn as %q and declares no share of the row", th.Text, th.Class)
		}
		widths[i] = tablePx * share / 100
		total += share
		if th.Text == "best box" {
			boxAt = i
		}
	}
	if math.Abs(total-100) > 0.5 {
		t.Errorf("the Mango columns take %g%% of the row, want 100%%, or the row does not fill the panel", total)
	}
	if boxAt < 0 {
		t.Fatal("the Mango header names no best box column")
	}
	return widths, boxAt
}

// mangoColumnName is the header text of the Mango table's column, for a
// failure that names the column rather than its index.
func mangoColumnName(t *testing.T, dom *pageDOM, i int) string {
	t.Helper()

	return dom.descend(t, "mango", 0, 0, 0, i).Text
}

// mangoTableWidth is the width the Mango table gets at a page viewport, read
// from the page's own layout: *{box-sizing:border-box} makes body's 1500px cap
// include its 16px of padding, .grid splits what is left after its 12px gap
// between its two tracks, and the Mango section's border and its .body's
// padding sit between the left column and the table. The Mango panel is in the
// left column.
func mangoTableWidth(t *testing.T, viewport float64) float64 {
	t.Helper()

	body := topRule(t, "body")
	content := math.Min(viewport, cssPxValue(t, declOf(t, body, "max-width"))) -
		boxSide(t, body, "padding", "left") - boxSide(t, body, "padding", "right")
	grid := topRule(t, ".grid")
	tracks := frTracks(t, declOf(t, grid, "grid-template-columns"))
	left := (content - cssPxValue(t, declOf(t, grid, "gap"))) * tracks[0] / (tracks[0] + tracks[1])
	return left - mangoPanelInset(t)
}

// mangoPanelInset is what the Mango section's border and its .body's padding
// take from the width the panel is given: the table is what is left of it.
func mangoPanelInset(t *testing.T) float64 {
	t.Helper()

	panel := topRule(t, "section")
	pad := topRule(t, ".body")
	return 2*cssPxValue(t, declOf(t, panel, "border")) + boxSide(t, pad, "padding", "left") + boxSide(t, pad, "padding", "right")
}

// mangoPanelReadWidth is the width the Mango table gets at the 1760px page the
// dashboard is read at: about 620px in the left column, under the width the page
// stacks it at. It is the panel's real width, and the width the stacked layout
// is measured at (gt-c6ge9).
func mangoPanelReadWidth(t *testing.T) float64 {
	t.Helper()

	return mangoTableWidth(t, 1760)
}

// mangoWidePanelWidth is a panel width above the stacked width, where the page
// draws its six-column table rather than stacking it: the two tests that guard
// that table measure it at this width (gt-c6ge9).
const mangoWidePanelWidth = 800

// mangoWideTableWidth is what the table fills of a panel that wide.
func mangoWideTableWidth(t *testing.T) float64 {
	t.Helper()

	return mangoWidePanelWidth - mangoPanelInset(t)
}

// mangoWideColumnWidths reads the columns as the six-column table draws them on
// a panel of mangoWidePanelWidth.
func mangoWideColumnWidths(t *testing.T, dom *pageDOM) ([]float64, int) {
	t.Helper()

	return mangoColumnWidths(t, dom, mangoWideTableWidth(t))
}

// requireWideTable fails when a panel of mangoWidePanelWidth stacks instead of
// drawing the six-column table, so a test that measures that table must also
// read the width the page stacks below.
func requireWideTable(t *testing.T) {
	t.Helper()

	if got := mangoStackWidth(t); float64(mangoWidePanelWidth) <= got {
		t.Fatalf("the Mango table stacks up to %gpx, so a %dpx panel draws no six-column table to measure", got, mangoWidePanelWidth)
	}
}

// mangoStackWidth is the container width at or below which the page stacks the
// Mango table, read from the @container rule that guards the stacked layout.
func mangoStackWidth(t *testing.T) float64 {
	t.Helper()

	css := pageStyle(t)
	const at = "@container (max-width:"
	for from := 0; ; {
		i := strings.Index(css[from:], at)
		if i < 0 {
			t.Fatal("the page has no @container (max-width: ...) rule that styles the Mango table, so it never stacks")
		}
		i += from
		start := i + len(at)
		width, block := strings.Index(css[start:], ")"), strings.Index(css[start:], "}")
		if width < 0 || block < 0 {
			t.Fatalf("the @container rule at offset %d is not written as a width and a block", i)
		}
		if strings.Contains(css[start:start+block], ".mgtable") {
			return cssPxValue(t, strings.TrimSpace(css[start:start+width]))
		}
		from = start + block
	}
}

// mangoStackedLines is the grid line each cell of a stacked target row draws on,
// by the class the cell carries. It mirrors the grid-area rules in the
// stylesheet's container block, so a test can say which cells share a line.
var mangoStackedLines = map[string]int{"jvname": 1, "mgverdict": 1, "mgrates": 2, "mginterval": 2, "mgevidence": 3, "mgbox": 3}

// mangoStackedCellWidth is the width a cell of a stacked target row draws in at
// the panel's real width: the table's width less the column gap between the
// row's two tracks, split evenly between them. The stacked cells carry no
// padding, so a track is all the room its cell's text has.
func mangoStackedCellWidth(t *testing.T) float64 {
	t.Helper()

	row := containerDecls(t, "max-width: 760px", ".mgtable tbody tr")
	tracks := frTracks(t, row["grid-template-columns"])
	if len(tracks) != 2 || tracks[0] != tracks[1] {
		t.Fatalf("the stacked target row draws %q, want two equal tracks", row["grid-template-columns"])
	}
	if got := containerDecls(t, "max-width: 760px", ".mgtable tbody td")["padding"]; got != "0" {
		t.Fatalf("the stacked cells draw with padding %q, want 0 so a track is all the room its text has", got)
	}
	return (mangoPanelReadWidth(t) - mangoPanelInset(t) - cssPxValue(t, row["column-gap"])) / 2
}

// mangoStackedRowHeight is the height a stacked target row draws at a cell width:
// each grid line is as tall as its tallest cell, the lines are set apart by the
// row gap, and the row adds its padding above and below and the rule above it.
// The first row draws without that rule and padding, so this reads the taller case.
func mangoStackedRowHeight(t *testing.T, row []mangoNode, base mangoMeasure, width float64) float64 {
	t.Helper()

	style := containerDecls(t, "max-width: 760px", ".mgtable tbody tr")
	lines := map[int]float64{}
	for _, cell := range row {
		line := 0
		for class, n := range mangoStackedLines {
			if mangoNamesClass(cell.Classes, class) {
				line = n
			}
		}
		if line == 0 {
			t.Fatalf("the stacked target row draws a cell on no grid line: %q", cell.Classes)
		}
		if h := mangoCellHeight(t, cell, base, width); h > lines[line] {
			lines[line] = h
		}
	}
	height := float64(len(lines)-1) * cssPxValue(t, style["row-gap"])
	for _, h := range lines {
		height += h
	}
	pad := cssPxValue(t, strings.Fields(style["padding"])[0])
	return height + 2*pad + cssPxValue(t, style["border-top"])
}

// mangoNode is one element the Mango table draws, with the classes of the
// elements it sits inside, outermost first, and the text its subtree holds.
type mangoNode struct {
	Tag     string      `json:"tag"`
	Classes []string    `json:"classes"`
	Open    bool        `json:"open"`
	Text    string      `json:"text"`
	All     string      `json:"all"`
	Kids    []mangoNode `json:"kids"`
}

// mangoText is the text style an element draws its content with: a white-space
// rule that keeps a line together, an overflow-wrap that lets one break
// anywhere, and the font size it sets. The properties inherit, so a node starts
// from its parent's and applies what its own classes declare.
type mangoText struct {
	whiteSpace string
	wrap       string
	font       float64
}

// mangoRun is one stretch of text the browser cannot break across lines, at the
// font size it draws it.
type mangoRun struct {
	Text string
	Font float64
}

// apply returns the style the node's classes give it, outermost class first, as
// the cascade serves an inner rule over the outer one it was written against.
func (s mangoText) apply(t *testing.T, classes []string) mangoText {
	t.Helper()

	for _, class := range classes {
		decls := cssDecls(t, class)
		if v, ok := decls["white-space"]; ok {
			s.whiteSpace = v
		}
		if v, ok := decls["overflow-wrap"]; ok {
			s.wrap = v
		}
		if v, ok := decls["font-size"]; ok {
			s.font = cssPxValue(t, v)
		}
	}
	return s
}

// runs is every stretch of the node's text the browser cannot break: a nowrap
// element keeps its whole content on one line, a leaf breaks anywhere when its
// style lets it (anywhere, or break-word, which breaks a word only once the word
// is wider than the line), and otherwise breaks at its longest word.
func (n mangoNode) runs(t *testing.T, s mangoText) []mangoRun {
	t.Helper()

	s = s.apply(t, n.Classes)
	if s.whiteSpace == "nowrap" && strings.TrimSpace(n.All) != "" {
		return []mangoRun{{Text: n.All, Font: s.font}}
	}
	if len(n.Kids) == 0 {
		text := strings.TrimSpace(n.Text)
		switch {
		case text == "":
			return nil
		case s.wrap == "anywhere" || s.wrap == "break-word":
			return []mangoRun{{Text: string([]rune(text)[:1]), Font: s.font}}
		}
		longest := ""
		for _, word := range strings.Fields(text) {
			if len([]rune(word)) > len([]rune(longest)) {
				longest = word
			}
		}
		return []mangoRun{{Text: longest, Font: s.font}}
	}
	var out []mangoRun
	for _, kid := range n.Kids {
		out = append(out, kid.runs(t, s)...)
	}
	return out
}

// mangoRows reads every body cell of the Mango table as the element tree the
// page built, so a test measures the row the page draws rather than the rules
// it draws it from.
func mangoRows(t *testing.T, dom *pageDOM) [][]mangoNode {
	t.Helper()

	const expr = `JSON.stringify((function () {
  var table = __byId["mango"].children.find(function (c) { return String(c.className).split(" ").indexOf("mgtable") >= 0; });
  return table.children[1].children.map(function (tr) {
    return tr.children.map(function (td) {
      return (function walk(n, chain) {
        var here = chain.concat(String(n.className || "").split(" ").filter(function (x) { return x; }));
        return {
          tag: String(n.tagName || ""),
          open: !!n.open,
          classes: here,
          text: n.children.length ? "" : String(n.textContent || ""),
          all: String(n.textContent || ""),
          kids: n.children.map(function (c) { return walk(c, here); })
        };
      })(td, []);
    });
  });
})())`
	v, err := dom.vm.RunString(expr)
	if err != nil {
		t.Fatalf("reading the Mango table's rows: %v", err)
	}
	var rows [][]mangoNode
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the Mango table's rows: %v", err)
	}
	return rows
}

// mangoRowNamed returns the body row whose name cell draws label, so a test
// measures the row it asked for wherever the table places it.
func mangoRowNamed(t *testing.T, rows [][]mangoNode, label string) []mangoNode {
	t.Helper()

	for _, row := range rows {
		if len(row) > 0 && strings.TrimSpace(row[0].All) == label {
			return row
		}
	}
	t.Fatalf("the Mango table draws no row named %q", label)
	return nil
}

// monospaceAdvance is one monospace character in ems. The page's font stack is
// monospace and Menlo, its widest named face, advances 0.6em, so measuring at
// 0.6em measures the text no narrower than any face draws it.
const monospaceAdvance = 0.6

// topRule returns one rule's declarations from the top level of the page's
// stylesheet. A rule inside an @media block is another viewport's layout and is
// skipped.
func topRule(t *testing.T, sel string) string {
	t.Helper()

	var found string
	for _, rule := range strings.Split(withoutMediaBlocks(pageStyle(t)), "}") {
		s, body, ok := strings.Cut(rule, "{")
		if ok && strings.TrimSpace(s) == sel {
			found = body
		}
	}
	if found == "" {
		t.Fatalf("index.html has no %s rule", sel)
	}
	return found
}

// declOf returns one property of a rule's declarations.
func declOf(t *testing.T, decls, prop string) string {
	t.Helper()

	for _, decl := range strings.Split(decls, ";") {
		if k, v, ok := strings.Cut(decl, ":"); ok && strings.TrimSpace(k) == prop {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no %s in %q", prop, decls)
	return ""
}

// boxSide returns one side of a box's padding or border: a longhand
// declaration wins over the shorthand, which expands as CSS writes it.
func boxSide(t *testing.T, decls, prop, side string) float64 {
	t.Helper()

	if strings.Contains(decls, prop+"-"+side+":") {
		return cssPxValue(t, declOf(t, decls, prop+"-"+side))
	}
	fields := strings.Fields(declOf(t, decls, prop))
	switch len(fields) {
	case 1:
		return cssPxValue(t, fields[0])
	case 2:
		fields = []string{fields[0], fields[1], fields[0], fields[1]}
	case 3:
		fields = []string{fields[0], fields[1], fields[2], fields[1]}
	}
	at := map[string]int{"top": 0, "right": 1, "bottom": 2, "left": 3}[side]
	return cssPxValue(t, fields[at])
}

// frTracks reads the share of each track in a grid-template-columns value.
func frTracks(t *testing.T, template string) []float64 {
	t.Helper()

	var out []float64
	for i := 0; i < len(template); i++ {
		if !strings.HasPrefix(template[i:], "fr") {
			continue
		}
		j := i
		for j > 0 && (template[j-1] >= '0' && template[j-1] <= '9' || template[j-1] == '.') {
			j--
		}
		n, err := strconv.ParseFloat(template[j:i], 64)
		if err != nil {
			t.Fatalf("reading the tracks of %q: %v", template, err)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		t.Fatalf("grid-template-columns %q declares no track", template)
	}
	return out
}

// cssPxValue reads a declaration's leading pixel measurement: "1500px" is 1500,
// "1px solid var(--line)" is 1, and a unitless "0" is 0 as CSS reads it.
func cssPxValue(t *testing.T, v string) float64 {
	t.Helper()

	if strings.TrimSpace(v) == "0" {
		return 0
	}
	end := strings.IndexFunc(v, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if end <= 0 || !strings.HasPrefix(v[end:], "px") {
		t.Fatalf("%q is not a pixel measurement", v)
	}
	n, err := strconv.ParseFloat(v[:end], 64)
	if err != nil {
		t.Fatalf("%q is not a pixel measurement: %v", v, err)
	}
	return n
}

// pageStyle is index.html's stylesheet with its comments removed, so a rule can
// be read without a comment's braces or semicolons interfering.
// withoutMediaBlocks returns css with every @media block removed, braces
// matched, so a rule nested in one cannot be read as a top-level rule.
func withoutMediaBlocks(css string) string {
	var out strings.Builder
	for {
		at := strings.Index(css, "@media")
		if at < 0 {
			out.WriteString(css)
			return out.String()
		}
		out.WriteString(css[:at])
		depth, end := 0, -1
		for i := at; i < len(css) && end < 0; i++ {
			switch css[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i + 1
				}
			}
		}
		if end < 0 {
			return out.String()
		}
		css = css[end:]
	}
}

func pageStyle(t *testing.T) string {
	t.Helper()

	page := string(indexHTML)
	open, close := strings.Index(page, "<style>"), strings.Index(page, "</style>")
	if open < 0 || close < open {
		t.Fatal("index.html has no style block")
	}
	css := page[open+len("<style>") : close]
	for {
		start := strings.Index(css, "/*")
		if start < 0 {
			return css
		}
		end := strings.Index(css[start:], "*/")
		if end < 0 {
			return css
		}
		css = css[:start] + css[start+end+2:]
	}
}

// cssDecls returns the declarations the page's stylesheet sets on class, keyed
// by property: every rule whose selector names the class contributes, later
// rules last, so a Mango rule written to override a panel-wide one is read the
// way the browser applies it. A test names the class the page draws rather
// than the text a selector is spelled in.
func cssDecls(t *testing.T, class string) map[string]string {
	t.Helper()

	decls := map[string]string{}
	for _, rule := range strings.Split(pageStyle(t), "}") {
		sel, body, ok := strings.Cut(rule, "{")
		if !ok || !selectsClass(sel, class) {
			continue
		}
		for _, decl := range strings.Split(body, ";") {
			prop, val, ok := strings.Cut(decl, ":")
			if !ok {
				continue
			}
			decls[strings.TrimSpace(prop)] = strings.TrimSpace(val)
		}
	}
	return decls
}

// selectsClass reports whether a selector's subject — its last compound, the
// element the rule styles — names class as a whole class: .mgtable .mgbox
// styles the box and not the table, and .mgboxes is not .mgbox.
func selectsClass(sel, class string) bool {
	for _, one := range strings.Split(sel, ",") {
		fields := strings.Fields(one)
		if len(fields) == 0 {
			continue
		}
		for _, name := range strings.Split(fields[len(fields)-1], ".") {
			if name == class {
				return true
			}
		}
	}
	return false
}

// mangoPageWeek is one week of the trend as the hub hands it to the page: the
// week's first day, its landings and the share of them rejected.
func mangoPageWeek(week string, landings int, share float64) map[string]any {
	return map[string]any{"week": week, "landings": landings, "rejected_share": share}
}

// mangoPageEvent is one regime event as the hub hands it to the page.
func mangoPageEvent(date, what string) map[string]any {
	return map[string]any{"date": date, "what": what}
}

// mustFloat reads a number the page drew as an attribute, failing on anything
// that is not one.
func mustFloat(t *testing.T, s string) float64 {
	t.Helper()

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("the page drew %q where a number belongs: %v", s, err)
	}
	return v
}

// mangoClassValues walks the elements the page drew under #mango and returns
// one JavaScript value from each whose class holds class, in draw order. An
// HTML element's class is the className property while the page sets an SVG
// element's class with setAttribute, so the walk reads both.
func mangoClassValues(t *testing.T, dom *pageDOM, class, value string) []string {
	t.Helper()

	expr := `(function () {
  var out = [];
  (function walk(n) {
    for (var i = 0; i < (n.children || []).length; i++) {
      var c = n.children[i];
      var cls = String(c.className || "") + " " + String(c.getAttribute("class") || "");
      if (cls.split(/\s+/).indexOf(` + quoteJS(class) + `) >= 0) out.push(` + value + `);
      walk(c);
    }
  })(__byId["mango"]);
  return out;
})()`
	v, err := dom.vm.RunString("JSON.stringify(" + expr + ")")
	if err != nil {
		t.Fatalf("reading the %q elements under #mango: %v", class, err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %q elements under #mango: %v", class, err)
	}
	return out
}

// mangoTexts reads the text of every element drawn under #mango whose class
// holds class.
func mangoTexts(t *testing.T, dom *pageDOM, class string) []string {
	t.Helper()

	return mangoClassValues(t, dom, class, "c.textContent")
}

// mangoElements reads back every element drawn under #mango whose class holds
// class, in draw order. A test finds a part by the class that marks it rather
// than by its position among its siblings or by its whole class list, either of
// which drawing order and styling can change.
func mangoElements(t *testing.T, dom *pageDOM, class string) []drawn {
	t.Helper()

	expr := `(function () {
  var out = [];
  (function walk(n) {
    for (var i = 0; i < (n.children || []).length; i++) {
      var c = n.children[i];
      var cls = String(c.className || "") + " " + String(c.getAttribute("class") || "");
      if (cls.split(/\s+/).indexOf(` + quoteJS(class) + `) >= 0) out.push({text: c.textContent, class: String(c.className || ""), kids: c.children.length});
      walk(c);
    }
  })(__byId["mango"]);
  return out;
})()`
	v, err := dom.vm.RunString("JSON.stringify(" + expr + ")")
	if err != nil {
		t.Fatalf("reading the %q elements under #mango: %v", class, err)
	}
	var out []drawn
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %q elements under #mango: %v", class, err)
	}
	return out
}

// hasClass reports whether the element wears class as one token of its class
// list: the page greys with that token, and the rest of the list is the
// stylesheet's business rather than a test's.
func hasClass(d drawn, class string) bool {
	for _, c := range strings.Fields(d.Class) {
		if c == class {
			return true
		}
	}
	return false
}

// mangoAttrs reads one attribute from every element drawn under #mango whose
// class holds class. The chart places its marks with attributes, which drawn
// does not carry.
func mangoAttrs(t *testing.T, dom *pageDOM, class, name string) []string {
	t.Helper()

	return mangoClassValues(t, dom, class, `String(c.getAttribute(`+quoteJS(name)+`) || "")`)
}

// mangoKids counts the children of every element drawn under #mango whose class
// holds class, so a test separates an element holding its text alone from one
// a render built a subtree into.
func mangoKids(t *testing.T, dom *pageDOM, class string) []int {
	t.Helper()

	got := mangoClassValues(t, dom, class, "String(c.children.length)")
	out := make([]int, 0, len(got))
	for _, s := range got {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("the %q element's child count reads %q: %v", class, s, err)
		}
		out = append(out, n)
	}
	return out
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

// Every rule inside an @media block is dropped, not only the first, so topRule
// never reads another viewport's rule as the page's own.
func TestWithoutMediaBlocksDropsEveryRuleInside(t *testing.T) {
	t.Parallel()
	css := "body{a:1} @media (max-width:600px){ .a{p:1} .b{p:2} } .c{p:3}"
	got := withoutMediaBlocks(css)
	for _, gone := range []string{".a", ".b"} {
		if strings.Contains(got, gone) {
			t.Errorf("withoutMediaBlocks kept %q from inside @media: %q", gone, got)
		}
	}
	for _, kept := range []string{"body{a:1}", ".c{p:3}"} {
		if !strings.Contains(got, kept) {
			t.Errorf("withoutMediaBlocks dropped top-level %q: %q", kept, got)
		}
	}
}

// mangoUnfitLines is the line count a box with no width reports for text: more
// than any limit the checks hold a cell to.
const mangoUnfitLines = 1 << 20

// mangoBaselineProblem is the check on the rate's baseline line: at most two
// lines in the column the rate leaves (gt-g4vif).
func mangoBaselineProblem(baseline string, lines int, width float64) string {
	if lines > 2 {
		return fmt.Sprintf("the baseline line %q takes %d lines in the %gpx the rate column leaves, want at most two", baseline, lines, width)
	}
	return ""
}

// mangoRowHeightProblem is the check on one cell of a row: a row may take 120px
// (gt-g4vif).
func mangoRowHeightProblem(column string, height float64, limit int) string {
	if height > float64(limit) {
		return fmt.Sprintf("the %s draws %gpx tall, over the %dpx a row may take", column, height, limit)
	}
	return ""
}

// TestMangoLayoutChecksFailWhereTheOldLayoutFailed drives the failing branch of
// each check the layout test applies: the baseline that breaks to three lines at
// 12px, a cell that stands over 120px at the page's 1.45 leading, and a column
// that has no width at all (gt-g4vif).
func TestMangoLayoutChecksFailWhereTheOldLayoutFailed(t *testing.T) {
	t.Parallel()

	const baseline = "vs 24.3% across all recent landings"
	old := mangoMeasure{wrap: "normal", font: 12, lhFactor: 1.45}
	fixed := mangoMeasure{wrap: "normal", font: 11, lhFactor: 1.2}
	if got := mangoLines(baseline, old, 115); got != 3 {
		t.Fatalf("the baseline at 12px in 115px takes %d lines, want the three the bead describes", got)
	}
	if got := mangoLines(baseline, fixed, 130); got != 2 {
		t.Fatalf("the baseline at 11px in 130px takes %d lines, want two", got)
	}
	if mangoBaselineProblem(baseline, mangoLines(baseline, old, 115), 115) == "" {
		t.Error("a baseline that takes three lines raises no problem")
	}
	if msg := mangoBaselineProblem(baseline, mangoLines(baseline, fixed, 130), 130); msg != "" {
		t.Errorf("a baseline that takes two lines raises %q", msg)
	}
	if mangoRowHeightProblem("rate", 4*old.lineBox()+60, 120) == "" {
		t.Error("a cell over 120px raises no problem")
	}
	if msg := mangoRowHeightProblem("rate", 4*fixed.lineBox()+60, 120); msg != "" {
		t.Errorf("a cell of %gpx raises %q", 4*fixed.lineBox()+60, msg)
	}
	if mangoBaselineProblem(baseline, mangoLines(baseline, fixed, 0), 0) == "" {
		t.Error("a column with no width is measured as fitting")
	}
	if mangoBaselineProblem(baseline, mangoLines(baseline, fixed, -4), -4) == "" {
		t.Error("a column with negative width is measured as fitting")
	}
}

// TestMangoStacksTheTableBelowTheSectionWidth: the Mango section is a size
// container, and under 760px a rule draws each target row as a grid so its
// cells land on lines of their own rather than in six columns the panel is too
// narrow for (gt-c6ge9).
func TestMangoStacksTheTableBelowTheSectionWidth(t *testing.T) {
	t.Parallel()

	if got := declOf(t, topRule(t, "#mangosec"), "container-type"); got != "inline-size" {
		t.Fatalf("the Mango section is a %q container, want inline-size so a rule can read the width the panel gets", got)
	}
	if got := containerDecls(t, "max-width: 760px", ".mgtable tbody tr")["display"]; got != "grid" {
		t.Errorf("a target row under the 760px container draws %q, want grid so each cell lands on its own line", got)
	}
	if got := containerDecls(t, "max-width: 760px", ".mgtable")["display"]; got != "block" {
		t.Errorf("the table under the 760px container draws %q, want block so its rows stack", got)
	}
}

// TestMangoEveryTargetRowCellNamesItsColumn: the stacked layout has no header
// above a cell to say what it holds, so every body cell of a target row carries
// its column's name in data-label, in the header's column order (gt-c6ge9).
func TestMangoEveryTargetRowCellNamesItsColumn(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	target := mangoPageTarget("rejected_before_landing", 48, false, "model = opus in q4", 96, 0.42, 0.25, MangoEarning)
	target["label"] = "Landings rejected before they land"
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", target)})

	var head []string
	for i := 0; i < 6; i++ {
		head = append(head, mangoColumnName(t, dom, i))
	}
	if got := mangoBodyLabels(t, dom, target["label"].(string)); fmt.Sprint(got) != fmt.Sprint(head) {
		t.Errorf("the target row's cells carry data-label %q, want each cell to name its column %q", got, head)
	}
}

// mangoBodyLabels reads the data-label of each cell in the body row whose name
// cell reads label, in the order the cells sit. The body's first row is the
// collection line, which is not a target row and names no columns.
func mangoBodyLabels(t *testing.T, dom *pageDOM, label string) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((function () {
  var table = __byId["mango"].children.find(function (c) { return String(c.className).split(" ").indexOf("mgtable") >= 0; });
  var row = table.children[1].children.find(function (tr) { return String(tr.children[0].textContent) === ` + quoteJS(label) + `; });
  if (!row) return [];
  return row.children.map(function (td) { return String(td.getAttribute("data-label") || ""); });
})())`)
	if err != nil {
		t.Fatalf("reading the target row's data-labels: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the target row's data-labels: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("the target row labelled %q is not drawn, so no cell names its column", label)
	}
	return out
}

// TestMangoVerdictChipStaysWhole: a verdict chip is one token, so it keeps its
// own line rather than splitting mid-word, and the verdict column's share is
// wide enough for it everywhere the table is drawn (gt-c6ge9).
//
// The column carries no min-width: under table-layout:fixed a browser ignores
// min-width on a column or cell (CSS 2.1 leaves it undefined), so the
// declaration looked like a floor and did nothing. The share is the floor: the
// six-column table is drawn only above the 760px stack width, where 14% is at
// least 106px.
func TestMangoVerdictChipStaysWhole(t *testing.T) {
	t.Parallel()

	chip := topRule(t, ".mgtable td.mgverdict .jvchip")
	if got := declOf(t, chip, "white-space"); got != "nowrap" {
		t.Errorf("a verdict chip draws with white-space %q, want nowrap so it is never split mid-word", got)
	}
	if got := declOf(t, chip, "overflow-wrap"); got != "normal" {
		t.Errorf("a verdict chip draws with overflow-wrap %q, want normal so it is never split mid-word", got)
	}
	decls := cssDecls(t, "mgc-verdict")
	if got, ok := decls["min-width"]; ok {
		t.Errorf("the verdict column declares min-width %q, which a fixed-layout table ignores: size the column with its width share", got)
	}
	share := strings.TrimSuffix(decls["width"], "%")
	pct, err := strconv.ParseFloat(share, 64)
	if err != nil {
		t.Fatalf("the verdict column's width %q is not a percentage: %v", decls["width"], err)
	}
	const narrowestWide, chipFloor = 760.0, 100.0 // px: the stack width, and room for the longest verdict chip
	if got := narrowestWide * pct / 100; got < chipFloor {
		t.Errorf("at the narrowest wide layout (%vpx) the verdict column is %.1fpx, want at least %vpx so its chip fits", narrowestWide, got, chipFloor)
	}
}

// TestMangoTargetNameBreaksOnlyInsideALongWord: a target name wraps between
// words, and breaks inside a word only when that word is longer than its column,
// so it is never broken anywhere (gt-c6ge9).
func TestMangoTargetNameBreaksOnlyInsideALongWord(t *testing.T) {
	t.Parallel()

	if got := cssDecls(t, "jvname")["overflow-wrap"]; got != "break-word" {
		t.Errorf("a target name draws with overflow-wrap %q, want break-word so it breaks only inside a word longer than its column", got)
	}
}

// TestMangoIntervalHeaderClipsAtItsCell: the interval column is narrower than
// the word it heads, so the header cell clips the word inside its own cell and
// the whole word rides in a title for a reader who hovers it (gt-c6ge9).
func TestMangoIntervalHeaderClipsAtItsCell(t *testing.T) {
	t.Parallel()

	head := cssDecls(t, "mgc-interval")
	if got := head["overflow"]; got != "hidden" {
		t.Errorf("the interval header draws with overflow %q, want hidden so the word clips inside its cell", got)
	}
	if got := head["text-overflow"]; got != "ellipsis" {
		t.Errorf("the interval header draws with text-overflow %q, want ellipsis so the clipped word ends in a mark", got)
	}
	if got := head["white-space"]; got != "nowrap" {
		t.Errorf("the interval header draws with white-space %q, want nowrap so its one word stays on one line", got)
	}

	dom := newPageDOM(t)
	dom.call(t, "renderMango", map[string]any{"mango": mangoPageState(MangoFresh, "fresh", mangoPageTarget("rejected_before_landing", 48, false, "model = opus in q4", 96, 0.42, 0.25, MangoEarning))})
	if got := mangoAttrs(t, dom, "mgc-interval", "title"); len(got) != 1 || got[0] != "interval" {
		t.Errorf("the interval header carries titles %q, want the whole word \"interval\"", got)
	}
}

// containerDecls returns the declarations the @container block guarded by cond
// sets on the rule whose selector is sel, keyed by property. A rule inside the
// block styles a descendant of the size container, so it is read from the block
// rather than from the top-level rules cssDecls reads (gt-c6ge9).
func containerDecls(t *testing.T, cond, sel string) map[string]string {
	t.Helper()

	css := pageStyle(t)
	at := strings.Index(css, "@container ("+cond+")")
	if at < 0 {
		t.Fatalf("the page has no @container (%s) rule", cond)
	}
	open := strings.Index(css[at:], "{")
	if open < 0 {
		t.Fatalf("the @container (%s) rule has no block", cond)
	}
	depth, end := 0, -1
	for i := at + open; i < len(css) && end < 0; i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		t.Fatalf("the @container (%s) rule is not closed", cond)
	}
	decls := map[string]string{}
	for _, rule := range strings.Split(css[at+open+1:end], "}") {
		s, body, ok := strings.Cut(rule, "{")
		if !ok || !namesSelector(s, sel) {
			continue
		}
		for _, decl := range strings.Split(body, ";") {
			prop, val, ok := strings.Cut(decl, ":")
			if !ok {
				continue
			}
			decls[strings.TrimSpace(prop)] = strings.TrimSpace(val)
		}
	}
	return decls
}

// namesSelector reports whether a rule's selector list names sel exactly, so a
// rule written for a group of selectors is read for each of them.
func namesSelector(list, sel string) bool {
	for _, one := range strings.Split(list, ",") {
		if strings.TrimSpace(one) == sel {
			return true
		}
	}
	return false
}
