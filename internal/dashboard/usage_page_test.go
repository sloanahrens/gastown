package dashboard

import (
	"strings"
	"testing"
)

// The Usage pane draws the payload the hub holds: both kinds of spend, the
// over-twenty flag, and the named states for an unreadable, stale or never
// scanned set of records. The page is data-driven, so these tests hold the
// function's source to what it reads and writes, as the Deep review pane's do.
func TestUsagePanelDrawsThePayload(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "costUsage")
	for _, want := range []string{
		`function costUsage(box, u, s) {`,
		// The panel's header reads without opening the part: the DeepSeek
		// estimate and the count of beads over twenty minutes.
		`byClass.filter(r => isDeepSeek(r.model))`,
		`text: "DeepSeek est. " + money(est) + " · " + over.length + (over.length === 1 ? " bead" : " beads") + " over 20m"`,
		`warn: over.length > 0,`,
		// Claude rows are tokens, DeepSeek rows are dollars labelled est.
		`isDeepSeek(r.model) ? "est. " + money(r.est_cost_usd) : fmtTokens(r.uncached) + " tokens"`,
		// A bead over twenty minutes is flagged in the page's warning style.
		`[r.bead, "warnc"]`,
		`r.test_runs + " runs, " + r.failed_test_runs + " failed"`,
		`const byClass = u.by_class_model || [], deep = u.deep_review_runs || [], top = u.top_beads || [], over = u.over20 || [];`,
		`(u.unmatched || 0) + " unmatched · " + (u.incomplete || 0) + " incomplete · " + (u.skipped || 0) + " skipped"`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("costUsage has no %q", want)
		}
	}

	// The usage part is drawn by the Cost panel, and no panel of its own is left.
	if cost := pageFunc(t, "renderCost"); !strings.Contains(cost, "costUsage(box, u, s)") {
		t.Error("the Cost panel does not draw the usage part")
	}
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderCost(state)") {
		t.Error("renderAll does not draw the Cost panel")
	}
	if !strings.Contains(string(indexHTML), `<section data-fold="cost"><h2>Cost <span class="r" id="costnote"></span></h2><div class="body" id="cost"></div></section>`) {
		t.Error("the Cost section is not the markup its neighbours are")
	}
}

// An unreadable record set, a stale scan and a scan that never ran each say so
// in words, and an unreadable set is never drawn as an empty list.
func TestUsagePanelNamesItsStates(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "costUsage")
	for _, want := range []string{
		`if (u.unavailable) {`,
		`box.append(el("div", "badc", "usage records unreadable"))`,
		`el("div", "empty warnc", "never scanned")`,
		`"records last scanned " + age(u.as_of) + " ago" + (old ? " (stale)" : "")`,
		`none("no usage in the last " + (u.window || "24h"))`,
		`none("no deep review runs")`,
		`none("no beads in the last " + (u.window || "24h"))`,
		`none("none in the last 48h")`,
		`section("Over 20 minutes (48h)", over.length`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("costUsage has no %q", want)
		}
	}

	// The unavailable branch returns before any list is drawn.
	branch := draw[strings.Index(draw, `if (u.unavailable) {`):]
	if ret, list := strings.Index(branch, "return {text:"), strings.Index(branch, "const byClass"); ret < 0 || list < 0 || ret > list {
		t.Error("an unreadable record set falls through to the lists")
	}

	// Stale is three hours.
	if !strings.Contains(string(indexHTML), `const USAGE_STALE_SEC = 3 * 3600;`) {
		t.Error("the stale threshold is not three hours")
	}
	if !strings.Contains(draw, `> USAGE_STALE_SEC`) {
		t.Error("costUsage does not compare the scan's age with the stale threshold")
	}
}

// The pane is collapsed on load and keeps the viewer's choice across redraws,
// the way the idle polecats section does.
func TestUsagePanelIsCollapsedByDefault(t *testing.T) {
	t.Parallel()

	if !strings.Contains(string(indexHTML), `let openUsage = false;`) {
		t.Error("the Usage pane does not start collapsed")
	}
	draw := pageFunc(t, "costUsage")
	for _, want := range []string{
		`document.createElement("details")`,
		`d.open = openUsage;`,
		`d.ontoggle = () => { openUsage = d.open; };`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("costUsage has no %q", want)
		}
	}
}

// A bead id, a model name and a rig are text, and a tag in them shows as that
// text and never as markup.
func TestUsagePanelSetsNoMarkup(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "costUsage")
	for _, bad := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(draw, bad) {
			t.Errorf("costUsage uses %s", bad)
		}
	}
	// Every cell is built through el(), which writes textContent, in the table
	// helper the Usage and rework parts share.
	if !strings.Contains(draw, `const table = costTable;`) {
		t.Error("costUsage does not draw its tables through costTable")
	}
	cells := pageFunc(t, "costTable")
	if !strings.Contains(cells, `const td = el("td", cls || "", text);`) || !strings.Contains(cells, "tr.append(td)") {
		t.Error("costTable does not draw its cells through el()")
	}
	for _, bad := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(cells, bad) {
			t.Errorf("costTable uses %s", bad)
		}
	}
}

// The Deep reviews table shows what a run's tokens bought: the findings the
// Deep review block's reader counted for the same bead, and the uncached tokens
// per finding. The two lists come from different readers, so the page joins
// them by bead id and does no new read.
func TestUsagePanelJoinsDeepReviewFindings(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "costUsage")
	for _, want := range []string{
		// The join: the Deep review block's runs keyed by their id, read off the
		// same state the Usage payload rides in.
		`const found = new Map(((s.deepreview && s.deepreview.runs) || []).map(r => [r.id, r.findings]));`,
		`const f = found.get(r.bead);`,
		`["Findings", 1], ["Tokens per finding", 1]`,
		// Findings: the count, and "-" for a run the Deep review pane does not list.
		`[f == null ? "-" : String(f), "n"]`,
		// Tokens per finding: uncached over findings, in the other token cells'
		// format, and "-" for zero or unknown findings.
		`[f ? fmtTokens(Math.round(r.uncached / f)) : "-", "n"]`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("costUsage has no %q", want)
		}
	}
}

// A run the Deep review pane does not list, a run it lists with no findings and
// a state with no deep review at all each draw "-" rather than a number, and
// the quotient is guarded by the count, so no case divides by zero. The page is
// data-driven, so these tests hold its expressions to those cases.
func TestUsagePanelDeepReviewColumnsHandleMissingAndZero(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "costUsage")
	// No deep review in the state, or one that carried no rows, joins to an
	// empty map: every run then draws "-" in both columns.
	if !strings.Contains(draw, `(s.deepreview && s.deepreview.runs) || []`) {
		t.Error("costUsage joins without guarding a missing deepreview or runs")
	}
	// Unknown findings (no entry) is told apart from zero findings (an entry of
	// 0): the first draws "-", the second draws "0".
	if !strings.Contains(draw, `f == null ? "-" : String(f)`) {
		t.Error("the Findings cell does not tell a missing run from a run with none")
	}
	// The quotient only runs for a truthy count: both undefined and 0 draw "-".
	if !strings.Contains(draw, `f ? fmtTokens(Math.round(r.uncached / f)) : "-"`) {
		t.Error("the Tokens per finding cell does not guard a zero or unknown count")
	}
	if strings.Count(draw, "r.uncached / ") != 1 {
		t.Error("costUsage divides uncached tokens somewhere other than the guarded cell")
	}
}
