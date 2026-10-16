package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
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
}

// drawCost runs renderCost over s and reads back the panel's lines and caption.
func drawCost(t *testing.T, s map[string]any) (lines []costLine, note drawn) {
	t.Helper()
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children.map(c => ({class: c.className, text: c.textContent})))`)
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
