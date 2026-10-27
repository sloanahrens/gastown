package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Cost panel is one section where DeepSeek and Usage were two, with the GCP
// budget line beside them. There is no JS engine in the unit tier, so like the
// other page tests these pin the lines the page draws from, not a rendered DOM
// (gt-qqn0s).

// The old DeepSeek and Usage panels are gone; the Cost panel keeps the shape
// the dashboard watch scans: a section whose h2 holds the title and a span.r.
func TestCostPanelReplacesTheDeepSeekAndUsagePanels(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if !strings.Contains(page, `<section><h2>Cost <span class="r" id="costnote"></span></h2><div class="body" id="cost"></div></section>`) {
		t.Error("the Cost section is not a section whose h2 holds the title and a span.r")
	}
	for _, gone := range []string{`<h2>DeepSeek `, `<h2>Usage `, `id="spendnote"`, `id="usagenote"`, `function renderSpend(`, `function renderUsage(`} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still carries %q from before the panels merged", gone)
		}
	}
	if strings.Count(page, "<h2>Cost ") != 1 {
		t.Error("the page does not carry exactly one Cost panel")
	}

	cost := pageFunc(t, "renderCost")
	for _, want := range []string{
		// All three parts, in the order the panel reads: DeepSeek, GCP, Usage.
		`costSpend(spendBox, d, r, s.cost_charts && s.cost_charts.headline)`,
		`costGCP(box, s.cloud)`,
		`costUsage(box, u, s)`,
		// Neither a spend report nor usage records is one line.
		`if (!d && !u) {`,
		`"no spend report"`,
	} {
		if !strings.Contains(cost, want) {
			t.Errorf("renderCost has no %q", want)
		}
	}
	if spend, gcp, usage := strings.Index(cost, "costSpend("), strings.LastIndex(cost, "costGCP("), strings.Index(cost, "costUsage("); !(spend < gcp && gcp < usage) {
		t.Error("renderCost does not draw DeepSeek, then the GCP line, then Usage")
	}
	// The one-line case returns before any part is drawn.
	one, parts := strings.Index(cost, `if (!d && !u) {`), strings.Index(cost, "costSpend(")
	if one < 0 || parts < 0 || one > parts || !strings.Contains(cost[one:parts], "return;") {
		t.Error("a panel with nothing to show goes on to draw its parts")
	}

	// The caption carries the usage glance and the spend report's own words,
	// and the caption is the span.r the watch reads.
	for _, want := range []string{
		`note.textContent = readCaption(r, [usage.text || null].concat(spendWords));`,
		`note.className === "r" && (usage.bad || usage.warn)`,
	} {
		if !strings.Contains(cost, want) {
			t.Errorf("renderCost does not write its caption: no %q", want)
		}
	}

	// Every part of the panel is drawn into the one box, so a panel's redraw
	// replaces them together; and the page tick still reaches it.
	if !strings.Contains(cost, `box.replaceChildren();`) {
		t.Error("renderCost does not clear the panel before it draws")
	}
	if all := pageFunc(t, "renderAll"); strings.Contains(all, "renderSpend") || strings.Contains(all, "renderUsage") {
		t.Error("renderAll still draws a panel that merged into Cost")
	}
}

// The GCP line reads the budget_alert findings the Cloud panel's reader carries
// and takes its colour from the finding's severity.
func TestCostPanelDrawsTheGCPBudgetLine(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "costGCP")
	for _, want := range []string{
		// The same findings the Cloud panel lists, picked by kind.
		`(c.findings || []).filter(f => f.kind === "budget_alert")`,
		// Project, spent of budget and percent, read back from the sentence.
		`spent[1] + " of " + spent[3] + " " + spent[2] + " · " + pct[1] + "%"`,
		`"GCP budget · " + (f.project ? f.project + " · " : "") + what`,
		// A sentence that does not match is shown whole, not dropped.
		`: detail || "alert"`,
		// Coloured by the finding's severity with the Cloud panel's own table.
		`CLOUD_SEV[f.severity] || "sev-low"`,
		// A late patrol is said on the line, since the figure may be old.
		`c.state === "overdue"`,
		`line.title = detail;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("costGCP has no %q", want)
		}
	}
	for _, bad := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(body, bad) {
			t.Errorf("costGCP uses %s", bad)
		}
	}

	// The patterns match the sentence the patrol writes today.
	page := string(indexHTML)
	for _, want := range []string{
		`const BUDGET_SPENT = /spent ([\d.,]+) ([A-Za-z]{3}) of its ([\d.,]+) [A-Za-z]{3} budget/;`,
		`const BUDGET_PCT = /(\d+(?:\.\d+)?)% of the budget/;`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page has no %q", want)
		}
	}

}

// costLine is one top-level child of the Cost panel as drawn.
type costLine struct {
	Class string `json:"class"`
	Text  string `json:"text"`
	Title string `json:"title"`
}

// drawCost runs renderCost over s and reads back the panel's lines and caption.
func drawCost(t *testing.T, s map[string]any) (lines []costLine, note drawn) {
	t.Helper()
	return drawCostOn(t, newPageDOM(t), s)
}

// drawCostOn runs renderCost over s on dom, a page the caller has already set
// up, and reads back the panel's lines and caption.
func drawCostOn(t *testing.T, dom *pageDOM, s map[string]any) (lines []costLine, note drawn) {
	t.Helper()
	dom.call(t, "renderCost", s)
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children.map(c => ({class: c.className, text: c.textContent, title: c.title})))`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	if err := json.Unmarshal([]byte(v.String()), &lines); err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	return lines, dom.element(t, "costnote")
}

func budgetFinding(severity string) map[string]any {
	return map[string]any{
		"kind": "budget_alert", "severity": severity, "project": "proj-a",
		"detail": "project proj-a has spent 4.57 USD of its 5.00 USD budget this month, 91% of the budget",
	}
}

// The budget line says what the patrol says when it can, and says it cannot see
// when it cannot: silence after a failed read would read as "under budget".
func TestCostPanelGCPLineByCloudState(t *testing.T) {
	t.Parallel()

	spend := map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}}
	cases := []struct {
		name      string
		cloud     map[string]any
		wantClass string // class of the GCP line; "" means none is drawn
		wantText  string
	}{
		{"alert", map[string]any{"state": "fresh", "findings": []any{budgetFinding("urgent")}},
			"sev-critical", "GCP budget · proj-a · 4.57 of 5.00 USD · 91%"},
		{"normal alert takes the amber", map[string]any{"state": "fresh", "findings": []any{budgetFinding("normal")}},
			"sev-medium", "GCP budget · proj-a · 4.57 of 5.00 USD · 91%"},
		{"alert from an overdue patrol", map[string]any{"state": "overdue", "findings": []any{budgetFinding("urgent")}},
			"sev-critical", "91% · patrol overdue"},
		{"other findings are not a budget line", map[string]any{"state": "fresh", "findings": []any{map[string]any{"kind": "idle_vm", "severity": "low"}}},
			"", ""},
		{"fresh and under the threshold", map[string]any{"state": "fresh"}, "", ""},
		{"read failed", map[string]any{"state": "error", "note": "reports unreadable"},
			"badc", "GCP budget · cloud read failed · reports unreadable"},
		{"overdue with no alert", map[string]any{"state": "overdue"},
			"warnc", "patrol overdue, no alert on its last report"},
		{"report missing", map[string]any{"state": "missing", "note": "no report"},
			"empty", "GCP budget · no patrol report"},
		{"no patrol on this machine", map[string]any{"state": "missing", "no_patrol": true}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, _ := drawCost(t, map[string]any{"spend": spend, "cloud": tc.cloud})
			var gcp []costLine
			for _, l := range lines {
				if strings.HasPrefix(l.Text, "GCP budget") {
					gcp = append(gcp, l)
				}
			}
			if tc.wantClass == "" {
				if len(gcp) != 0 {
					t.Fatalf("drew a GCP line %+v, want none", gcp)
				}
				return
			}
			if len(gcp) != 1 || gcp[0].Class != tc.wantClass || !strings.Contains(gcp[0].Text, tc.wantText) {
				t.Fatalf("GCP lines = %+v, want one of class %q containing %q", gcp, tc.wantClass, tc.wantText)
			}
		})
	}
}

// With neither a spend report nor usage there is one line, the cloud's budget
// alert still shows, and the caption never says "undefined".
func TestCostPanelEmptyStateAndCaption(t *testing.T) {
	t.Parallel()

	// No spend command configured, usage not scanned yet: one honest line.
	lines, note := drawCost(t, map[string]any{})
	if len(lines) != 1 || !strings.Contains(lines[0].Text, "no spend report") || !strings.Contains(lines[0].Text, "usage still reading") {
		t.Errorf("empty panel = %+v, want one line naming the spend report and the usage read", lines)
	}
	if note.Text != "" {
		t.Errorf("empty panel caption = %q, want none", note.Text)
	}

	// The same panel with a budget alert keeps the alert.
	lines, _ = drawCost(t, map[string]any{"cloud": map[string]any{"state": "fresh", "findings": []any{budgetFinding("urgent")}}})
	if len(lines) != 2 || !strings.HasPrefix(lines[1].Text, "GCP budget") {
		t.Errorf("empty panel with an alert = %+v, want the line and the alert", lines)
	}

	// Usage not yet answered beside a spend report: the caption is the report's
	// own words, with no undefined in it.
	spend := map[string]any{"balance": 12.5, "last_sample": "2026-10-15T10:00:00", "live": map[string]any{"ok": true}}
	_, note = drawCost(t, map[string]any{"spend": spend})
	if strings.Contains(note.Text, "undefined") || strings.Contains(note.Text, "null") || !strings.Contains(note.Text, "ledger 2026-10-15 10:00:00") {
		t.Errorf("caption with usage absent = %q", note.Text)
	}

	// Unreadable usage reaches the caption in the page's failure colour.
	_, note = drawCost(t, map[string]any{"spend": spend, "usage": map[string]any{"unavailable": true}})
	if !strings.Contains(note.Text, "usage unreadable") || !strings.Contains(note.Class, "badc") {
		t.Errorf("caption with unreadable usage = %q (%s)", note.Text, note.Class)
	}
}

// The DeepSeek, usage and GCP parts each name their own failure rather than
// borrowing another's, and the usage part's failure reaches the caption.
func TestCostPanelNamesEachPartsState(t *testing.T) {
	t.Parallel()

	usage := pageFunc(t, "costUsage")
	for _, want := range []string{
		`return {text: "usage unreadable", bad: true};`,
		`box.append(el("div", "empty", "reading…")); return {};`,
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("costUsage has no %q", want)
		}
	}
	spend := pageFunc(t, "costSpend")
	for _, want := range []string{
		`"unavailable — the spend read failed" : "no spend report"`,
		`live balance unreachable; showing the ledger`,
		`spend watcher is not running`,
		// The caption words the spend report brings stay: ledger sample, read age.
		`"ledger " + d.last_sample.replace("T", " ")`,
		`"read " + fmtDur(r.ageSec) + " ago"`,
	} {
		if !strings.Contains(spend, want) {
			t.Errorf("costSpend has no %q", want)
		}
	}

	// The grey the spend read earns covers its own lines only.
	if !strings.Contains(string(indexHTML), `.body.stale,#cost .spendpart.stale{opacity:.55}`) {
		t.Error("the spend lines are not greyed by a stale read")
	}
	if !strings.Contains(pageFunc(t, "renderCost"), `el("div", "spendpart")`) {
		t.Error("the spend lines are not in the block the grey applies to")
	}
}

// Every field the DeepSeek panel showed is still drawn: the tiles, the cash and
// credit split, the credit runway, the per-day line, the credits line and the
// sampled-since label. Only the grouping changed.
func TestCostPanelKeepsEveryDeepSeekField(t *testing.T) {
	t.Parallel()

	spend := pageFunc(t, "costSpend")
	for _, want := range []string{
		`add(money(d.balance), "DeepSeek balance"`,
		`"DeepSeek spent (sampled" + (since ? " since " + since[0] : "") + ")"`,
		`add(money(d.spent_total), "DeepSeek spent since top-up")`,
		`"DeepSeek last 1h"`,
		`"DeepSeek last 24h"`,
		`"DeepSeek runway"`,
		`money(d.cash) + " cash + " + money(d.credit) + " credit"`,
		`"credit lasts ~" + cr.toFixed(1) + "d, then cash"`,
		`Object.entries(d.per_day || {})`,
		`d.credits.slice().reverse()`,
	} {
		if !strings.Contains(spend, want) {
			t.Errorf("costSpend lost a DeepSeek field: no %q", want)
		}
	}
}

// perDay is a spend report's per_day map, one entry per day from a fixed base.
// The report hands the recent days and the panel reads the last key as today,
// so a test builds the history it reads back rather than pinning a date.
func perDay(days ...any) map[string]any {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	m := map[string]any{}
	for i, v := range days {
		m[base.AddDate(0, 0, i).Format("2006-01-02")] = map[string]any{"spent": v}
	}
	return m
}

// dimLines reads back the spend part's dim lines in the order it draws them:
// the cash and credit split, the day line, the credit events. The tiles above
// them are the kv row and the warnings below wear their own colour, so a test
// that reads one of these is not thrown by its neighbours.
func dimLines(t *testing.T, spend map[string]any) []costLine {
	t.Helper()
	dom := newPageDOM(t)
	dom.call(t, "renderCost", map[string]any{"spend": spend})
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children[0].children.filter(c => c.className === "title").map(c => ({class: c.className, text: c.textContent, title: c.title})))`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	var lines []costLine
	if err := json.Unmarshal([]byte(v.String()), &lines); err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	return lines
}

// spendReport is a spend report whose balance the two fields split, with the
// rest of the report the panel needs to draw the lines it reads.
func spendReport(cash, credit float64, runway any) map[string]any {
	return map[string]any{
		"balance": cash + credit, "live": map[string]any{"ok": true},
		"cash": cash, "credit": credit, "credit_runway_days_at_24h_pace": runway,
	}
}

// Cash, credit and the credit runway are one line under the balance: the split
// says how much of the balance is not money paid in and the runway says when
// the credit part runs out, which is one reading (gt-53s6m).
func TestCostPanelPutsCashCreditAndTheRunwayOnOneLine(t *testing.T) {
	t.Parallel()

	lines := dimLines(t, spendReport(5.2, 17.98, 1.1))
	if len(lines) != 1 {
		t.Fatalf("the spend part drew %d dim lines: %+v, want the split alone", len(lines), lines)
	}
	want := "$5.20 cash + $17.98 credit · credit lasts ~1.1d, then cash"
	if lines[0].Text != want {
		t.Errorf("the line under the balance = %q, want %q", lines[0].Text, want)
	}

	// Credit spent down to nothing: the split stays and the runway goes, since
	// there is no credit left to run out.
	lines = dimLines(t, spendReport(5.2, 0, 0))
	if len(lines) != 1 || lines[0].Text != "$5.20 cash + $0.00 credit" {
		t.Errorf("a report with no credit = %+v, want the split with no runway", lines)
	}

	// The split is drawn under the tiles, which stay as they were.
	panel, _ := drawCost(t, map[string]any{"spend": spendReport(5.2, 17.98, 1.1)})
	if !strings.Contains(panel[0].Text, "balance") || !strings.Contains(panel[0].Text, "runway") {
		t.Errorf("the balance, spend and rate tiles = %q, want them kept above the split", panel[0].Text)
	}
}

// The day line reads today and yesterday off the per_day history and puts the
// mean of the complete days behind them between, so one heavy day does not read
// as the rate; the hover carries every day the report sent, the mean's own
// window included (gt-53s6m).
func TestCostPanelDayLineAveragesTheLastSevenCompleteDays(t *testing.T) {
	t.Parallel()

	// Nine days: today, yesterday and seven complete ones. The eighth day back
	// is outside the mean and in the hover all the same.
	lines := dimLines(t, map[string]any{
		"balance": 1.0, "live": map[string]any{"ok": true},
		"per_day": perDay(1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0),
	})
	if len(lines) != 1 {
		t.Fatalf("the spend part drew %d dim lines: %+v, want the day line alone", len(lines), lines)
	}
	want := "today $9.00 · yesterday $8.00 · 7-day avg $5.00/day"
	if lines[0].Text != want {
		t.Errorf("the day line = %q, want %q", lines[0].Text, want)
	}
	if strings.Contains(lines[0].Text, "day)") || strings.Contains(lines[0].Text, "days)") {
		t.Errorf("a full week's mean wears a short-history note: %q", lines[0].Text)
	}
	if !strings.HasPrefix(lines[0].Title, "03-09 $9.00 · ") || !strings.Contains(lines[0].Title, "03-01 $1.00") {
		t.Errorf("the day line's hover = %q, want every day, newest first", lines[0].Title)
	}
}

// A history shorter than a week says how many days its mean covers, a day the
// report listed without a figure reads as a dash rather than as nothing spent,
// a negative day reads as the balance rise it is, and a rise is left out of the
// mean rather than read as a cheaper rate (gt-53s6m, gt-3ux9j).
func TestCostPanelDayLineWordsAShortHistory(t *testing.T) {
	t.Parallel()

	day := func(days ...any) costLine {
		t.Helper()
		lines := dimLines(t, map[string]any{
			"balance": 1.0, "live": map[string]any{"ok": true}, "per_day": perDay(days...),
		})
		if len(lines) != 1 {
			t.Fatalf("the spend part drew %d dim lines: %+v, want the day line alone", len(lines), lines)
		}
		return lines[0]
	}

	// Four days: today, yesterday and two complete ones.
	if got, want := day(1.0, 2.0, 4.0, 3.0).Text, "today $3.00 · yesterday $4.00 · 7-day avg $2.33/day (3 days)"; got != want {
		t.Errorf("a four-day history = %q, want %q", got, want)
	}
	// Two days: yesterday is the one complete day the mean is over, worded in
	// the singular.
	if got, want := day(1.0, 2.0).Text, "today $2.00 · yesterday $1.00 · 7-day avg $1.00/day (1 day)"; got != want {
		t.Errorf("a two-day history = %q, want %q", got, want)
	}
	// Today with no figure reads as a dash; the days behind it still average.
	if got, want := day(1.0, 2.0, nil).Text, "today – · yesterday $2.00 · 7-day avg $1.50/day (2 days)"; got != want {
		t.Errorf("a day with no figure = %q, want %q", got, want)
	}
	// Today alone: no yesterday to name, and nothing behind today to average, so
	// neither the yesterday figure nor a mean is drawn (gt-3ux9j).
	if got, want := day(3.0).Text, "today $3.00 · yesterday –"; got != want {
		t.Errorf("a one-day history = %q, want %q", got, want)
	}
	// A negative day is a balance rise here, as on the credits line.
	rise := day(1.0, 2.0, 3.0, -37.33)
	if got, want := rise.Text, "today +$37.33 added · yesterday $3.00 · 7-day avg $2.00/day (3 days)"; got != want {
		t.Errorf("a history with a rise today = %q, want %q", got, want)
	}
	if !strings.Contains(rise.Title, "03-04 +$37.33 added") {
		t.Errorf("the day line's hover = %q, want the rise worded as one", rise.Title)
	}
	// A rise inside the window is not a day's spend, so the mean is taken over
	// the spend days alone rather than dragged below the rate they show
	// (gt-3ux9j).
	if got, want := day(1.0, -37.33, 3.0, 4.0).Text, "today $4.00 · yesterday $3.00 · 7-day avg $2.00/day (2 days)"; got != want {
		t.Errorf("a rise in the mean's window = %q, want %q", got, want)
	}
	// Every day behind today rose: there is no spend to average, so the mean is
	// left off rather than drawn as a rate the report did not send (gt-3ux9j).
	if got, want := day(-1.0, -2.0, 3.0).Text, "today $3.00 · yesterday +$2.00 added"; got != want {
		t.Errorf("a window of rises = %q, want %q", got, want)
	}
}

// No per_day at all draws no day line: a line of dashes would be a heading over
// nothing, and the panel is read for the balance and the rate without it.
func TestCostPanelDayLineNeedsAHistory(t *testing.T) {
	t.Parallel()

	for _, spend := range []map[string]any{
		{"balance": 1.0, "live": map[string]any{"ok": true}},
		{"balance": 1.0, "live": map[string]any{"ok": true}, "per_day": map[string]any{}},
	} {
		if lines := dimLines(t, spend); len(lines) != 0 {
			t.Errorf("a report with no per_day drew a day line: %+v", lines)
		}
	}
}

// The credits line carries the two newest balance rises with the kind the
// report gave them, and the hover the older ones, so the line does not grow
// with the ledger (gt-53s6m).
func TestCostPanelCreditsLineShowsTheTwoNewestRises(t *testing.T) {
	t.Parallel()

	rise := func(at string, amount float64, kind any) map[string]any {
		e := map[string]any{"at": at, "amount": amount}
		if kind != nil {
			e["kind"] = kind
		}
		return e
	}
	// The report lists its rises oldest first; the line leads with the newest.
	added := rise("2026-09-29T20:50:00", 99.98, nil)
	topUp := rise("2026-10-05T11:02:00", 25.0, "top-up")
	credit := rise("2026-10-15T04:04:00", 37.33, "credit")
	line := func(rises ...map[string]any) costLine {
		t.Helper()
		list := make([]any, 0, len(rises))
		for _, r := range rises {
			list = append(list, r)
		}
		lines := dimLines(t, map[string]any{
			"balance": 1.0, "live": map[string]any{"ok": true}, "credits": list,
		})
		if len(lines) != 1 {
			t.Fatalf("the spend part drew %d dim lines: %+v, want the credits line alone", len(lines), lines)
		}
		return lines[0]
	}

	// Three rises: the two newest on the line, the oldest in its hover.
	three := line(added, topUp, credit)
	if want := "10-15 04:04 +$37.33 credit · 10-05 11:02 +$25.00 top-up"; three.Text != want {
		t.Errorf("three rises = %q, want the two newest", three.Text)
	}
	if want := "older rises · 09-29 20:50 +$99.98 added"; three.Title != want {
		t.Errorf("the rise that did not fit = %q, want it in the hover", three.Title)
	}

	// Two rises fit whole, so the hover holds nothing rather than repeating
	// them; one rise with no kind at all reads as "added".
	if two := line(topUp, credit); two.Text != "10-15 04:04 +$37.33 credit · 10-05 11:02 +$25.00 top-up" || two.Title != "" {
		t.Errorf("two rises = %q (%q), want both on the line and no hover", two.Text, two.Title)
	}
	if one := line(added); one.Text != "09-29 20:50 +$99.98 added" || one.Title != "" {
		t.Errorf("one rise = %q (%q), want it alone on the line", one.Text, one.Title)
	}
}

// claudePlan is a Claude plan block as the reader answers it, with the fields in
// over replacing or (on a nil value) dropping the reader's own: one week at 38%
// that resets in two days and is projected past its limit at the day's pace, the
// reading the panel draws a bad-coloured line from.
func claudePlan(now time.Time, over map[string]any) map[string]any {
	p := map[string]any{
		"weekly_pct":               38.0,
		"five_hour_pct":            9.0,
		"weekly_resets_at":         now.Add(52 * time.Hour).Unix(),
		"five_hour_resets_at":      now.Add(3 * time.Hour).Unix(),
		"pace_per_day_1h":          3.1,
		"pace_per_day_24h":         9.2,
		"projected_pct_at_reset":   104.0,
		"hits_limit_in_days":       6.1,
		"plan_usd_month":           200.0,
		"weekly_value_usd":         46.05,
		"used_value_usd":           17.5,
		"api_equivalent_week_usd":  31.4,
		"api_equivalent_month_usd": 120.05,
		"sampled_at":               now.Unix(),
		"stale":                    false,
	}
	for k, v := range over {
		if v == nil {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	return p
}

// resetText is how the page words a rollover: the local weekday and clock time.
// A page test reads it back from the same instant rather than pinning a weekday
// that moves with the day the suite runs.
func resetText(at time.Time) string { return at.Local().Format("Mon 15:04") }

// The plan line is the panel's second line: the DeepSeek part, the Claude plan
// line directly under it, then the GCP alert and the usage fold, so the two
// spend meters are read side by side (gt-qn3j4).
func TestCostPanelDrawsTheClaudePlanLineUnderTheDeepSeekLines(t *testing.T) {
	t.Parallel()

	now := time.Now()
	reset := now.Add(52 * time.Hour)
	lines, _ := drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}},
		"claude_plan": claudePlan(now, nil),
		"cloud":       map[string]any{"state": "fresh", "findings": []any{budgetFinding("urgent")}},
		"usage":       map[string]any{},
	})
	if len(lines) < 3 {
		t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
	}
	if !strings.Contains(lines[0].Text, "balance") {
		t.Errorf("the first line is not the DeepSeek part: %+v", lines[0])
	}
	want := "Claude plan · $200/mo · 38% of this week's limit · resets " + resetText(reset) +
		" · +0.13%/h (~$0.06/h) last 1h · +0.38%/h (~$0.18/h) last 24h" +
		" · projected 104% at reset · limit in 6.1d"
	if lines[1].Text != want {
		t.Errorf("the line under the DeepSeek part = %q, want %q", lines[1].Text, want)
	}
	// The panel's own colours, and the badge it carries where it is not plain.
	if lines[1].Class != "badc" {
		t.Errorf("a week projected past its limit wears %q, want badc", lines[1].Class)
	}
	if !strings.HasPrefix(lines[2].Text, "GCP budget") {
		t.Errorf("the line after the plan line = %q, want the GCP alert", lines[2].Text)
	}

	// The panel's redraw draws it in this order, on both paths it can take: a
	// panel with neither a spend report nor usage still owes the plan line.
	cost := pageFunc(t, "renderCost")
	spend := strings.Index(cost, "const spendWords = costSpend(")
	if spend < 0 {
		t.Fatal("renderCost has no drawn panel: no costSpend call behind the spend words")
	}
	plan, gcp := strings.Index(cost[spend:], "costClaudePlan("), strings.Index(cost[spend:], "costGCP(")
	if plan < 0 || gcp < 0 || plan >= gcp {
		t.Error("the drawn panel does not go DeepSeek, then the Claude plan line, then the GCP line")
	}
	if n := strings.Count(cost, "costClaudePlan("); n != 2 {
		t.Errorf("renderCost draws the plan line on %d paths, want the drawn panel and its one-line empty state", n)
	}
}

// The plan line is drawn from its own reader: with no spend report at all it is
// still drawn, on its own line, under the line that says the panel has nothing
// else.
func TestCostPanelClaudePlanLineDrawsWithoutASpendReport(t *testing.T) {
	t.Parallel()

	now := time.Now()
	lines, _ := drawCost(t, map[string]any{"claude_plan": claudePlan(now, nil)})
	if len(lines) != 2 {
		t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
	}
	if !strings.Contains(lines[0].Text, "no spend report") {
		t.Errorf("the empty panel's first line = %q, want the missing spend report", lines[0].Text)
	}
	if !strings.HasPrefix(lines[1].Text, "Claude plan · ") {
		t.Errorf("the empty panel's second line = %q, want the plan", lines[1].Text)
	}
}

// The line's colour is the panel's verdict on the week: bad at the ceiling the
// month rollup counts (90% of the limit) or projected past it, a warning past
// 90%, plain with room left.
func TestCostPanelClaudePlanLineColours(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cases := []struct {
		name string
		over map[string]any
		want string
	}{
		{"projected past the limit", map[string]any{"projected_pct_at_reset": 104.0}, "badc"},
		{"a week at the ceiling", map[string]any{"weekly_pct": 90.0, "projected_pct_at_reset": 95.0}, "badc"},
		{"projected past 90%", map[string]any{"weekly_pct": 40.0, "projected_pct_at_reset": 95.0}, "warnc"},
		{"reaching the limit is not passing it", map[string]any{"weekly_pct": 40.0, "projected_pct_at_reset": 100.0}, "warnc"},
		{"projected at 90%", map[string]any{"weekly_pct": 40.0, "projected_pct_at_reset": 90.0}, ""},
		{"a week with room", map[string]any{"weekly_pct": 40.0, "projected_pct_at_reset": 80.0, "hits_limit_in_days": nil}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, _ := drawCost(t, map[string]any{
				"spend":       map[string]any{"balance": 12.5},
				"claude_plan": claudePlan(now, tc.over),
			})
			if len(lines) < 2 {
				t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
			}
			if lines[1].Class != tc.want {
				t.Errorf("the plan line wears %q, want %q (%s)", lines[1].Class, tc.want, lines[1].Text)
			}
		})
	}
}

// A week the reader could not place is a grey line that says which state it is
// in, never a week at 0%: the numbers a stale series would carry are the ones
// that read as an untouched limit.
func TestCostPanelClaudePlanLineNeverDrawsAWeekItCouldNotRead(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cases := []struct {
		name string
		over map[string]any
		want string
	}{
		{"the samples could not be read", map[string]any{"unavailable": true, "weekly_pct": 0.0, "used_value_usd": 0.0},
			"Claude plan · unavailable — the status line's samples could not be read"},
		{"no samples yet", map[string]any{"stale": true, "sampled_at": nil, "weekly_pct": 0.0},
			"Claude plan · unavailable — the status line has written no samples yet"},
		{"a sample from a week that has reset", map[string]any{"stale": true, "weekly_resets_at": now.Add(-time.Hour).Unix()},
			"Claude plan · unavailable — the week the newest sample measures has already reset"},
		{"an old sample", map[string]any{"stale": true, "sampled_at": now.Add(-45 * time.Minute).Unix()},
			"Claude plan · unavailable — the newest sample is 45m old"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, _ := drawCost(t, map[string]any{
				"spend":       map[string]any{"balance": 12.5},
				"claude_plan": claudePlan(now, tc.over),
			})
			if len(lines) < 2 {
				t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
			}
			if lines[1].Class != "empty" {
				t.Errorf("the plan line wears %q, want the panel's grey (empty)", lines[1].Class)
			}
			if lines[1].Text != tc.want {
				t.Errorf("the plan line = %q, want %q", lines[1].Text, tc.want)
			}
			// No percentage and no dollar at all, so no zero of a week nobody read.
			if strings.ContainsAny(lines[1].Text, "%$") {
				t.Errorf("a week the reader could not place draws a number: %q", lines[1].Text)
			}
			if lines[1].Title != "" {
				t.Errorf("the plan line carries a hover (%q) over a week it does not draw", lines[1].Title)
			}
		})
	}
}

// The line's hover carries what it has no room for: the five-hour limit and its
// reset, the hour's pace, and the share of the plan the week has used, labelled
// approximate because it is the plan's arithmetic and not a bill (gt-qn3j4).
func TestCostPanelClaudePlanHoverCarriesTheFiveHourLimitAndThePace(t *testing.T) {
	t.Parallel()

	now := time.Now()
	lines, _ := drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, nil),
	})
	if len(lines) < 2 {
		t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
	}
	for _, want := range []string{
		"5-hour limit 9% used, resets " + resetText(now.Add(3*time.Hour)),
		"1-hour pace +3.1%/day",
		"about $17.50 of this week's $46.05, approximate",
	} {
		if !strings.Contains(lines[1].Title, want) {
			t.Errorf("the plan line's hover has no %q: %q", want, lines[1].Title)
		}
	}

	// A five-hour reading with no reset instant beside it says so rather than
	// dropping the limit from the hover.
	lines, _ = drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, map[string]any{"five_hour_resets_at": nil}),
	})
	if !strings.Contains(lines[1].Title, "5-hour limit 9% used, no reset instant") {
		t.Errorf("the hover of a reading with no reset instant = %q", lines[1].Title)
	}
}

// The hover also carries what the town's own Claude tokens would have cost on
// the API, over the week and the billing month: drawn beside the flat plan price
// the line shows, the saving is visible. Both are approximations - a plan
// weights tokens differently from the meter - and the hover says so rather than
// reading as a bill (gt-d8sem).
func TestCostPanelClaudePlanHoverCarriesTheAPIEquivalentDollars(t *testing.T) {
	t.Parallel()

	now := time.Now()
	lines, _ := drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, nil),
	})
	if len(lines) < 2 {
		t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
	}
	want := "API-equivalent $31.40 this week, $120.05 this month, approximate: plans weight tokens differently from the API"
	if !strings.Contains(lines[1].Title, want) {
		t.Errorf("the plan line's hover has no %q: %q", want, lines[1].Title)
	}

	// A model the price file has no rates for leaves the figures a floor, and
	// the hover names what it could not price.
	lines, _ = drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, map[string]any{"unpriced": []any{"claude-opus-9-9"}}),
	})
	if !strings.Contains(lines[1].Title, "counted at zero, no list price: claude-opus-9-9") {
		t.Errorf("the hover of a payload with unpriced models = %q, want them named", lines[1].Title)
	}

	// A payload with no figures behind it - the usage records could not be read
	// - draws no dollars at all: $0.00 there would read as a month that spent
	// nothing.
	lines, _ = drawCost(t, map[string]any{
		"spend": map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, map[string]any{
			"api_equivalent_week_usd": nil, "api_equivalent_month_usd": nil,
		}),
	})
	if strings.Contains(lines[1].Title, "API-equivalent") {
		t.Errorf("the hover prices a month the reader could not read: %q", lines[1].Title)
	}
}

// The line names no subscription: the series carries limits and a price, not a
// plan, so a line at the reader's default price is worded as any other.
func TestCostPanelClaudePlanLineNamesNoSubscription(t *testing.T) {
	t.Parallel()

	now := time.Now()
	for _, price := range []float64{ClaudePlanDefaultUSD, 100.0} {
		lines, _ := drawCost(t, map[string]any{
			"spend":       map[string]any{"balance": 12.5},
			"claude_plan": claudePlan(now, map[string]any{"plan_usd_month": price}),
		})
		if len(lines) < 2 {
			t.Fatalf("the panel drew %d lines for a $%v plan: %+v", len(lines), price, lines)
		}
		if want := "Claude plan · $" + strconv.Itoa(int(price)) + "/mo · "; !strings.HasPrefix(lines[1].Text, want) {
			t.Errorf("a plan at $%v = %q, want %q", price, lines[1].Text, want)
		}
	}
}

// The line's own words for the week: the day count when the limit is coming,
// "at the limit" for a week already spent (the reader sends 0 rather than a day
// count there), nothing when the limit outlives the window, and a signed pace
// that reads as falling when the week falls.
func TestCostPanelClaudePlanLineWordsTheWeek(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cases := []struct {
		name string
		over map[string]any
		want []string
		gone []string
	}{
		{"a limit inside the week", nil, []string{"· limit in 6.1d"}, nil},
		{"a week already at the limit", map[string]any{"weekly_pct": 107.0, "hits_limit_in_days": 0.0},
			[]string{"107% of this week's limit", "· at the limit"}, []string{"limit in"}},
		{"a limit beyond the window", map[string]any{"hits_limit_in_days": nil, "projected_pct_at_reset": 80.0},
			[]string{"projected 80% at reset"}, []string{"limit in", "at the limit"}},
		{"a falling week", map[string]any{"pace_per_day_24h": -1.2, "projected_pct_at_reset": 30.0, "hits_limit_in_days": nil},
			[]string{"-0.05%/h", "last 24h"}, []string{"limit in", "at the limit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, _ := drawCost(t, map[string]any{
				"spend":       map[string]any{"balance": 12.5},
				"claude_plan": claudePlan(now, tc.over),
			})
			if len(lines) < 2 {
				t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
			}
			for _, want := range tc.want {
				if !strings.Contains(lines[1].Text, want) {
					t.Errorf("the plan line has no %q: %q", want, lines[1].Text)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(lines[1].Text, gone) {
					t.Errorf("the plan line still carries %q: %q", gone, lines[1].Text)
				}
			}
		})
	}
}

// Both paces again as an hour's worth, with what that hour of plan use is worth
// beside it, so the Claude meter answers the DeepSeek line's own hourly pace. A
// window with no sample of that age draws a dash rather than a zero that reads
// as a limit nothing is being spent against, and neither figure moves the line's
// colour, which stays the projection's (gt-d2sy4).
func TestCostPanelClaudePlanLineGivesTheHourlyPace(t *testing.T) {
	t.Parallel()

	now := time.Now()
	plan := func(over map[string]any) costLine {
		t.Helper()
		lines, _ := drawCost(t, map[string]any{
			"spend":       map[string]any{"balance": 12.5},
			"claude_plan": claudePlan(now, over),
		})
		if len(lines) < 2 {
			t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
		}
		return lines[1]
	}

	// 3.1% of the week a day is 0.13% an hour, worth a hundredth of the week's
	// $46.05 for each point; 9.2% a day is 0.38% and $0.18 an hour.
	line := plan(nil)
	for _, want := range []string{"+0.13%/h (~$0.06/h) last 1h", "+0.38%/h (~$0.18/h) last 24h"} {
		if !strings.Contains(line.Text, want) {
			t.Errorf("the plan line has no %q: %q", want, line.Text)
		}
	}
	// The hover prices those dollars and leaves the colour to the projection,
	// which are the two things the line has no room to say.
	for _, want := range []string{
		"plan value at $0.46 a point of the week",
		"the colour comes from the projection: 104% at reset, past the limit bad, past 90% a warning, as a week already at 90% of the limit is",
	} {
		if !strings.Contains(line.Title, want) {
			t.Errorf("the plan line's hover has no %q: %q", want, line.Title)
		}
	}

	// A window with no sample of its age, one the reader measured flat to the
	// tenth, and one whose pace rounds to no points an hour all draw the dash
	// for their own window: the page cannot tell the flat week from the empty
	// window, and of the two errors a zero is the one that says the limit is
	// untouched.
	for _, tc := range []struct {
		over   map[string]any
		window string
	}{
		{map[string]any{"pace_per_day_1h": nil}, "last 1h"},
		{map[string]any{"pace_per_day_1h": 0.0}, "last 1h"},
		{map[string]any{"pace_per_day_24h": 0.0}, "last 24h"},
		{map[string]any{"pace_per_day_1h": 0.04}, "last 1h"},
		{map[string]any{"pace_per_day_1h": -0.04}, "last 1h"},
	} {
		flat := plan(tc.over)
		if !strings.Contains(flat.Text, "– "+tc.window) {
			t.Errorf("a plan with %v has no dash beside %s: %q", tc.over, tc.window, flat.Text)
		}
		if strings.Contains(flat.Text, "0.00%/h") {
			t.Errorf("a plan with %v draws a zero: %q", tc.over, flat.Text)
		}
	}
	// A payload without the week's price draws the percentages alone rather than
	// a dollar the reader never sent.
	if priceless := plan(map[string]any{"weekly_value_usd": nil}); !strings.Contains(priceless.Text, "+0.13%/h last 1h") {
		t.Errorf("a plan with no week's value = %q, want the pace without dollars", priceless.Text)
	}

	// The colour is the projection's: a week projected past its limit is bad
	// whatever its hourly figures say, and a heavy hour inside a week that is
	// not projected past the ceiling leaves the line plain.
	heavy := map[string]any{"pace_per_day_1h": 240.0, "pace_per_day_24h": 240.0}
	if past := plan(heavy); past.Class != "badc" {
		t.Errorf("a week projected past its limit wears %q, want badc: %q", past.Class, past.Text)
	}
	heavy["projected_pct_at_reset"] = 80.0
	if plain := plan(heavy); plain.Class != "" {
		t.Errorf("a plain week with a heavy hour wears %q, want no colour: %q", plain.Class, plain.Text)
	}
}

// planMonthFields is the month rollup's fields as the reader reports them: a
// billing month at 71% of the plan over three weeks, one of them at the ceiling
// and one still running, with the month before it at 54%. over replaces them or,
// on a nil value, drops them.
func planMonthFields(now time.Time, over map[string]any) map[string]any {
	m := map[string]any{
		"month_start_at":             now.AddDate(0, 0, -28).Unix(),
		"month_end_at":               now.AddDate(0, 0, 1).Unix(),
		"month_value_used_usd":       138.15,
		"month_utilization_pct":      71.0,
		"weeks_at_ceiling":           1,
		"avg_weekly_pct":             46.0,
		"month_weeks":                3,
		"month_open_week":            true,
		"prev_month_utilization_pct": 54.0,
	}
	for k, v := range over {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return m
}

// The plan line carries the billing month on its tail: where the plan stands for
// the month and how many weeks ran to the ceiling, which is the pair that sizes
// it. The line has no room for the month's own numbers — which month it is, what
// it comes to, the mean week and the month before — so they are on the hover
// (gt-bcq4m).
func TestCostPanelClaudePlanLineCarriesTheMonthSuffix(t *testing.T) {
	t.Parallel()

	now := time.Now()
	start, end := now.AddDate(0, 0, -28), now.AddDate(0, 0, 1)
	lines, _ := drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, planMonthFields(now, nil)),
	})
	if len(lines) < 2 {
		t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
	}
	if want := "· month 71% of plan · 1 week at ceiling"; !strings.HasSuffix(lines[1].Text, want) {
		t.Errorf("the plan line = %q, want it to end %q", lines[1].Text, want)
	}
	for _, want := range []string{
		"billing month " + start.Format("2 Jan") + " to " + end.Format("2 Jan") + ", the plan's renewal day to the next",
		"about $138.15 of the $200.00 plan, approximate",
		"mean week 46% over 3 weeks",
		"the week in progress counts at its latest reading",
		"month before 54% of plan",
	} {
		if !strings.Contains(lines[1].Title, want) {
			t.Errorf("the plan line's hover has no %q: %q", want, lines[1].Title)
		}
	}
}

// A state error from the reader reaches the hover, so a history that stopped
// being kept is visible on the page.
func TestCostPanelClaudePlanHoverShowsAStateError(t *testing.T) {
	t.Parallel()

	now := time.Now()
	lines, _ := drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, planMonthFields(now, map[string]any{"state_error": "weekly history: disk full"})),
	})
	if len(lines) < 2 || !strings.Contains(lines[1].Title, "state not kept: weekly history: disk full") {
		t.Errorf("the plan line's hover does not show the state error: %+v", lines)
	}
}

// The month's own words, case by case: the ceiling clause counts the month's
// weeks and is left out when none of them reached it, and the month before is
// named only when the history reaches back to one. A month of no counted weeks
// draws no clause at all — the reader has no history yet — since a month at 0%
// of the plan would read as a subscription nobody has used.
func TestCostPanelClaudePlanMonthSuffixWords(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cases := []struct {
		name      string
		over      map[string]any
		want      []string
		gone      []string
		hoverGone []string
	}{
		{
			name: "one week at the ceiling",
			want: []string{"· month 71% of plan · 1 week at ceiling"},
		},
		{
			name: "two weeks at the ceiling",
			over: map[string]any{"weeks_at_ceiling": 2},
			want: []string{"· month 71% of plan · 2 weeks at ceiling"},
			gone: []string{"1 week at ceiling"},
		},
		{
			name: "a month with no week at the ceiling",
			over: map[string]any{"weeks_at_ceiling": 0},
			want: []string{"· month 71% of plan"},
			gone: []string{"at ceiling"},
		},
		{
			name:      "a month with no month before it",
			over:      map[string]any{"prev_month_utilization_pct": nil},
			want:      []string{"· month 71% of plan"},
			hoverGone: []string{"month before"},
		},
		{
			name:      "a month already closed",
			over:      map[string]any{"month_open_week": false},
			want:      []string{"· month 71% of plan"},
			hoverGone: []string{"in progress"},
		},
		{
			name:      "no month to report",
			over:      map[string]any{"month_weeks": 0},
			gone:      []string{"month 71% of plan", "at ceiling"},
			hoverGone: []string{"billing month", "mean week", "month before"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, _ := drawCost(t, map[string]any{
				"spend":       map[string]any{"balance": 12.5},
				"claude_plan": claudePlan(now, planMonthFields(now, tc.over)),
			})
			if len(lines) < 2 {
				t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
			}
			for _, want := range tc.want {
				if !strings.Contains(lines[1].Text, want) {
					t.Errorf("the plan line has no %q: %q", want, lines[1].Text)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(lines[1].Text, gone) {
					t.Errorf("the plan line still carries %q: %q", gone, lines[1].Text)
				}
			}
			for _, gone := range tc.hoverGone {
				if strings.Contains(lines[1].Title, gone) {
					t.Errorf("the plan line's hover still carries %q: %q", gone, lines[1].Title)
				}
			}
		})
	}
}

// The page's ceiling is the reader's (ClaudePlanCeilingPct), off a constant that
// mirrors it: a change to either that left the other behind would colour a week
// by one threshold and count the month's ceiling weeks by another.
func TestCostPanelClaudePlanCeilingTracksTheReaderCeiling(t *testing.T) {
	t.Parallel()

	want := "const CLAUDE_PLAN_CEILING_PCT = " + strconv.Itoa(int(ClaudePlanCeilingPct)) + ";"
	if !strings.Contains(string(indexHTML), want) {
		t.Errorf("the page does not carry %q, so its week colour has drifted from the reader's ceiling", want)
	}
}

// A number the reader has not sent draws a dash for itself: a field left out of
// the payload is not a zero, and "NaN%" or "undefined" on the line would read as
// a measurement nobody took.
func TestCostPanelClaudePlanLineDrawsNoNaNForAMissingNumber(t *testing.T) {
	t.Parallel()

	now := time.Now()
	lines, _ := drawCost(t, map[string]any{
		"spend": map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, planMonthFields(now, map[string]any{
			"weekly_pct": nil, "projected_pct_at_reset": nil, "pace_per_day_24h": nil,
			"pace_per_day_1h": nil, "five_hour_pct": nil, "plan_usd_month": nil,
			"weekly_resets_at": nil, "five_hour_resets_at": nil, "used_value_usd": nil,
			"weekly_value_usd": nil, "month_start_at": nil, "month_end_at": nil,
			"month_utilization_pct": nil, "avg_weekly_pct": nil,
		})),
	})
	if len(lines) < 2 {
		t.Fatalf("the panel drew %d lines: %+v", len(lines), lines)
	}
	for _, got := range []string{lines[1].Text, lines[1].Title} {
		if strings.Contains(got, "NaN") || strings.Contains(got, "undefined") {
			t.Errorf("a payload missing a number drew %q", got)
		}
	}
}
