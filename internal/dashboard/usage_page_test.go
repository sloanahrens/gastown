package dashboard

import (
	"os/exec"
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

// The Deep reviews table shows what a run's tokens bought: the findings the
// Deep review pane counted for the same bead, and the uncached tokens per
// finding. The two lists come from different readers, so the page joins them by
// bead id and does no new read.
func TestUsagePanelJoinsDeepReviewFindings(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderUsage")
	for _, want := range []string{
		// The join: the Deep review pane's rows keyed by their id, read off the
		// same state the Usage payload rides in.
		`const found = new Map(((s.deepreview && s.deepreview.rows) || []).map(r => [r.id, r.findings]));`,
		`const f = found.get(r.bead);`,
		`["Findings", 1], ["Tokens per finding", 1]`,
		// Findings: the count, and "-" for a run the Deep review pane does not list.
		`[f == null ? "-" : String(f), "n"]`,
		// Tokens per finding: uncached over findings, in the other token cells'
		// format, and "-" for zero or unknown findings.
		`[f ? fmtTokens(Math.round(r.uncached / f)) : "-", "n"]`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderUsage has no %q", want)
		}
	}
}

// A run the Deep review pane lists with no findings, a run it does not list and
// a state with no deep review at all each draw "-" and never divide by zero,
// and a joined run draws its count and its quotient. The join expressions are
// run against those four shapes.
func TestUsagePanelDeepReviewColumnsCases(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `
const fmtTokens = n => {
  n = Number(n) || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(n);
};
const cells = (s, r) => {
  ` + deepReviewJoin(t) + `
  const f = found.get(r.bead);
  return [f == null ? "-" : String(f), f ? fmtTokens(Math.round(r.uncached / f)) : "-"];
};
const withRows = { deepreview: { rows: [{ id: "a", findings: 4 }, { id: "z", findings: 0 }] } };
console.log(JSON.stringify([
  cells(withRows, { bead: "a", uncached: 2500000 }),
  cells(withRows, { bead: "a", uncached: 10 }),
  cells(withRows, { bead: "z", uncached: 500 }),
  cells(withRows, { bead: "missing", uncached: 500 }),
  cells({ deepreview: null }, { bead: "a", uncached: 500 }),
  cells({}, { bead: "a", uncached: 500 }),
]));`
	out, err := exec.Command(node, "-e", script).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `[["4","625.0k"],["4","3"],["0","-"],["-","-"],["-","-"],["-","-"]]`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("columns = %s, want %s", got, want)
	}
}

// deepReviewJoin returns the page's own join statement, so the case test runs
// the code the page runs.
func deepReviewJoin(t *testing.T) string {
	t.Helper()

	draw := pageFunc(t, "renderUsage")
	i := strings.Index(draw, "const found = ")
	if i < 0 {
		t.Fatal("renderUsage has no deep review join")
	}
	line := draw[i:]
	return line[:strings.Index(line, "\n")]
}
