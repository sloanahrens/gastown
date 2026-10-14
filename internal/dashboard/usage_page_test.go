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

	draw := pageFunc(t, "renderUsage")
	for _, want := range []string{
		`const u = s.usage;`,
		// The header reads without opening the pane: the DeepSeek estimate and
		// the count of beads over twenty minutes.
		`byClass.filter(r => isDeepSeek(r.model))`,
		`note.textContent = "est. " + money(est) + " · " + over.length + " over 20m"`,
		// Claude rows are tokens, DeepSeek rows are dollars labelled est.
		`isDeepSeek(r.model) ? "est. " + money(r.est_cost_usd) : fmtTokens(r.uncached) + " tokens"`,
		// A bead over twenty minutes is flagged in the page's warning style.
		`[r.bead, "warnc"]`,
		`r.test_runs + " runs, " + r.failed_test_runs + " failed"`,
		`const byClass = u.by_class_model || [], deep = u.deep_review_runs || [], top = u.top_beads || [], over = u.over20 || [];`,
		`(u.unmatched || 0) + " unmatched · " + (u.incomplete || 0) + " incomplete · " + (u.skipped || 0) + " skipped"`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderUsage has no %q", want)
		}
	}

	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderUsage(state)") {
		t.Error("renderAll does not draw the Usage panel")
	}
	if !strings.Contains(string(indexHTML), `<section><h2>Usage <span class="r" id="usagenote"></span></h2><div class="body" id="usage"></div></section>`) {
		t.Error("the Usage section is not the markup its neighbours are")
	}
}

// An unreadable record set, a stale scan and a scan that never ran each say so
// in words, and an unreadable set is never drawn as an empty list.
func TestUsagePanelNamesItsStates(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderUsage")
	for _, want := range []string{
		`if (u.unavailable) {`,
		`box.append(el("div", "badc", "usage records unreadable"))`,
		`el("div", "empty warnc", "never scanned")`,
		`"records last scanned " + age(u.as_of) + " ago" + (old ? " (stale)" : "")`,
		`none("no usage in the last " + (u.window || "24h"))`,
		`none("no deep review runs")`,
		`none("no beads in the last " + (u.window || "24h"))`,
		`none("none in the last week")`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderUsage has no %q", want)
		}
	}

	// The unavailable branch returns before any list is drawn.
	branch := draw[strings.Index(draw, `if (u.unavailable) {`):]
	if ret, list := strings.Index(branch, "return;"), strings.Index(branch, "const byClass"); ret < 0 || list < 0 || ret > list {
		t.Error("an unreadable record set falls through to the lists")
	}

	// Stale is three hours.
	if !strings.Contains(string(indexHTML), `const USAGE_STALE_SEC = 3 * 3600;`) {
		t.Error("the stale threshold is not three hours")
	}
	if !strings.Contains(draw, `> USAGE_STALE_SEC`) {
		t.Error("renderUsage does not compare the scan's age with the stale threshold")
	}
}

// The pane is collapsed on load and keeps the viewer's choice across redraws,
// the way the idle polecats section does.
func TestUsagePanelIsCollapsedByDefault(t *testing.T) {
	t.Parallel()

	if !strings.Contains(string(indexHTML), `let openUsage = false;`) {
		t.Error("the Usage pane does not start collapsed")
	}
	draw := pageFunc(t, "renderUsage")
	for _, want := range []string{
		`document.createElement("details")`,
		`d.open = openUsage;`,
		`d.ontoggle = () => { openUsage = d.open; };`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderUsage has no %q", want)
		}
	}
}

// A bead id, a model name and a rig are text, and a tag in them shows as that
// text and never as markup.
func TestUsagePanelSetsNoMarkup(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderUsage")
	for _, bad := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(draw, bad) {
			t.Errorf("renderUsage uses %s", bad)
		}
	}
	// Every cell is built through el(), which writes textContent.
	if !strings.Contains(draw, `tr.append(el("td", cls || "", text))`) {
		t.Error("renderUsage does not draw its cells through el()")
	}
}
