package dashboard

import (
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
		`"no spend or usage report"`,
	} {
		if !strings.Contains(cost, want) {
			t.Errorf("renderCost has no %q", want)
		}
	}
	if spend, gcp, usage := strings.Index(cost, "costSpend("), strings.Index(cost, "costGCP("), strings.Index(cost, "costUsage("); !(spend < gcp && gcp < usage) {
		t.Error("renderCost does not draw DeepSeek, then the GCP line, then Usage")
	}
	// The one-line case returns before any part is drawn.
	if one, parts := strings.Index(cost, `if (!d && !u) {`), strings.Index(cost, "costSpend("); one > parts || !strings.Contains(cost[one:parts], "return;") {
		t.Error("a panel with nothing to show goes on to draw its parts")
	}

	// The caption carries the usage glance and the spend report's own words,
	// and the caption is the span.r the watch reads.
	for _, want := range []string{
		`note.textContent = readCaption(r, [usage.text].concat(spendWords));`,
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
		`((c && c.findings) || []).filter(f => f.kind === "budget_alert")`,
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

	// No finding, no line: the function appends only inside its loop.
	if strings.Count(body, "box.append(") != 1 || !strings.Contains(body[strings.Index(body, "for (const f of rows)"):], "box.append(line);") {
		t.Error("costGCP draws something when there is no budget_alert finding")
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
