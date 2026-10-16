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
		`costSpend(spendBox, d, r)`,
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
	if one, parts := strings.Index(cost, `if (!d && !u) {`), strings.Index(cost, "costSpend("); one > parts || !strings.Contains(cost[one:parts], "return;") {
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
	dom := newPageDOM(t)
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
		`add(money(d.balance), "balance"`,
		`"spent (sampled" + (since ? " since " + since[0] : "") + ")"`,
		`add(money(d.spent_total), "spent since top-up")`,
		`"last 1h"`,
		`"last 24h"`,
		`"runway"`,
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
	want := "Claude Max · $200/mo · 38% of this week's limit · resets " + resetText(reset) +
		" · +9.2%/day (24h) · projected 104% at reset · limit in 6.1d"
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
	plan := strings.Index(cost[spend:], "costClaudePlan(")
	gcp := strings.Index(cost[spend:], "costGCP(")
	if spend < 0 || !(0 <= plan && plan < gcp) {
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
	if !strings.HasPrefix(lines[1].Text, "Claude Max · ") {
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

// The page names the plan from its price, off a constant that mirrors the
// reader's default (ClaudePlanDefaultUSD): a change to the default that left the
// page behind would call a $100 plan Max.
func TestCostPanelClaudePlanNameTracksTheReaderDefaultPrice(t *testing.T) {
	t.Parallel()

	want := "const CLAUDE_PLAN_MAX_USD = " + strconv.Itoa(int(ClaudePlanDefaultUSD)) + ";"
	if !strings.Contains(string(indexHTML), want) {
		t.Errorf("the page does not carry %q, so its plan name has drifted from the reader's default price", want)
	}

	now := time.Now()
	lines, _ := drawCost(t, map[string]any{
		"spend":       map[string]any{"balance": 12.5},
		"claude_plan": claudePlan(now, map[string]any{"plan_usd_month": 100.0}),
	})
	if len(lines) < 2 || !strings.HasPrefix(lines[1].Text, "Claude plan · $100/mo · ") {
		t.Errorf("a plan at another price = %q, want it named for what it is", lines[1].Text)
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
			[]string{"-1.2%/day (24h)"}, []string{"limit in", "at the limit"}},
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
