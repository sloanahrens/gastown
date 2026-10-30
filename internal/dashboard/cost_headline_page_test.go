package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// headerPart is one span of the Cost panel's header as drawn: the words it
// shows, the class that colours them, and its tooltip.
type headerPart struct {
	Text  string `json:"text"`
	Class string `json:"class"`
	Title string `json:"title"`
}

// headerParts is the Cost header's spans in the order it drew them. The page
// joins them with text nodes, which the stub keeps as children, so only
// elements are read back.
func headerParts(t *testing.T, dom *pageDOM) []headerPart {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(__byId["costnote"].children.filter(c => c.nodeType !== 3).map(c => ({text: c.textContent, class: c.className, title: c.title})))`)
	if err != nil {
		t.Fatalf("reading the Cost header: %v", err)
	}
	var parts []headerPart
	if err := json.Unmarshal([]byte(v.String()), &parts); err != nil {
		t.Fatalf("reading the Cost header: %v", err)
	}
	return parts
}

// partByText is the header span whose words contain want, failing when the
// line drew none: a figure that went missing reads as a failure, not as an
// empty string.
func partByText(t *testing.T, parts []headerPart, want string) headerPart {
	t.Helper()

	for _, p := range parts {
		if strings.Contains(p.Text, want) {
			return p
		}
	}
	t.Fatalf("the header has no part saying %q: %+v", want, parts)
	return headerPart{}
}

// hasPartText reports whether some header span says want, for a figure the
// header must leave out.
func hasPartText(parts []headerPart, want string) bool {
	for _, p := range parts {
		if strings.Contains(p.Text, want) {
			return true
		}
	}
	return false
}

// liveSpend is a spend report that is present and current, so a header test
// does not also draw the "no spend read" part it is not about.
func liveSpend() map[string]any {
	return map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}}
}

// headerState is a reading whose rows carry every figure the header draws:
// DeepSeek's runway, the Claude week's percent and its limit projection, today
// on both, and a GCP budget alert.
func headerState(t *testing.T) map[string]any {
	t.Helper()

	return jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"cost_providers": []any{
			map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "today": 0.42, "runway_days": 7.74, "status": "ok", "status_text": "7.7d runway"},
			map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "today": 4.29, "plan_pct": 38.0, "status": "warn", "status_text": "limit in 2.6d"},
			map[string]any{"id": "jev", "label": "Jev", "kind": "cash"},
		},
		"cloud": map[string]any{"state": "fresh", "findings": []any{budgetFinding("normal")}},
	})
}

// The header is the few figures the operator reads first, each in the colour
// its own row judges it by: today's spend, DeepSeek's runway, the Claude week
// with the day its limit goes, and the GCP budget.
func TestCostHeaderShowsTheFiguresTheOperatorReads(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, headerState(t))
	parts := headerParts(t, dom)

	for _, want := range []string{
		"today $0.42 cash · $4.29 plan est.",
		"DeepSeek 7.7d runway",
		"Claude 38% (wk) limit in 2.6d",
		"GCP 91% of $5",
	} {
		partByText(t, parts, want)
	}
	const want = "today $0.42 cash · $4.29 plan est. · DeepSeek 7.7d runway · Claude 38% (wk) limit in 2.6d · GCP 91% of $5"
	if note := dom.element(t, "costnote"); note.Text != want {
		t.Errorf("the header reads %q, want %q", note.Text, want)
	}

	// Each figure wears the colour its row's status has in the table: ok is the
	// green class, warn the amber one, and the budget alert's normal severity
	// the amber the GCP line under the table draws it in.
	for text, want := range map[string]string{
		"today":  "",
		"runway": "good",
		"Claude": "warnc",
		"GCP":    "warnc",
	} {
		if got := partByText(t, parts, text).Class; got != want {
			t.Errorf("the %s part wears %q, want %q", text, got, want)
		}
	}
}

// Cash spent and the Claude plan's estimate are different money, so today's
// total names each rather than adding them under one word.
func TestCostHeaderNamesCashAndThePlanEstimateApart(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"cost_providers": []any{
			map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "today": 0.42},
			map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "today": 4.29, "plan_pct": 38.0, "status": "ok"},
		},
	}))
	if got := partByText(t, headerParts(t, dom), "today").Text; got != "today $0.42 cash · $4.29 plan est." {
		t.Errorf("today reads %q, want cash and the plan estimate named apart", got)
	}

	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"cost_providers": []any{
			map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "today": 4.29, "plan_pct": 38.0, "status": "ok"},
		},
	}))
	if got := partByText(t, headerParts(t, dom), "today").Text; got != "today $4.29 plan est." {
		t.Errorf("a plan-only today reads %q, want it named as the plan estimate", got)
	}
}

// The DeepSeek estimate over the usage window is a figure the header has always
// drawn, so it stays: it is the usage scan's own sum, shown with its window on
// hover.
func TestCostHeaderKeepsTheDeepSeekEstimate(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"usage": map[string]any{
			"as_of":  time.Now().UTC().Format(time.RFC3339),
			"window": "24h",
			"by_class_model": []any{
				map[string]any{"model": "deepseek-chat", "est_cost_usd": 1.2},
				map[string]any{"model": "deepseek-reasoner", "est_cost_usd": 0.3},
				map[string]any{"model": "claude-opus", "est_cost_usd": 9.0},
			},
		},
	}))
	est := partByText(t, headerParts(t, dom), "DeepSeek est.")
	if est.Text != "DeepSeek est. $1.50" {
		t.Errorf("the estimate reads %q, want the DeepSeek rows summed", est.Text)
	}
	if !strings.Contains(est.Title, "24h") {
		t.Errorf("the estimate's tooltip = %q, want its window", est.Title)
	}
}

// Each cloud state says what it is: a budget read that failed is named as
// unreadable, a patrol with no report as no data, and a figure from an overdue
// patrol is marked stale. A town with no cloud patrol draws no GCP part.
func TestCostHeaderNamesTheCloudStateItCannotShow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, want, class string
		cloud             map[string]any
	}{
		{"unreadable", "GCP: unreadable", "badc", map[string]any{"state": "error", "note": "the budget export failed"}},
		{"no report", "GCP: no data", "warnc", map[string]any{"state": "missing"}},
		{"fresh, no alert", "GCP: no alert", "", map[string]any{"state": "fresh"}},
		{"overdue, no alert", "GCP: no alert (stale)", "warnc", map[string]any{"state": "overdue"}},
		{"alert without figures", "GCP: alert, no figures", "warnc", map[string]any{"state": "fresh", "findings": []any{
			map[string]any{"kind": "budget_alert", "severity": "normal", "detail": "a budget alert with no numbers in it"},
		}}},
		{"overdue figure", "GCP 91% of $5 (stale)", "warnc", map[string]any{"state": "overdue", "findings": []any{budgetFinding("normal")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			drawCostOn(t, dom, jsonValue(t, map[string]any{"spend": liveSpend(), "cloud": tc.cloud}))
			got := partByText(t, headerParts(t, dom), "GCP")
			if got.Text != tc.want || got.Class != tc.class {
				t.Errorf("the GCP part reads %q in %q, want %q in %q", got.Text, got.Class, tc.want, tc.class)
			}
		})
	}

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"cloud": map[string]any{"state": "missing", "no_patrol": true},
	}))
	if parts := headerParts(t, dom); hasPartText(parts, "GCP") {
		t.Errorf("a town with no cloud patrol drew a GCP part: %+v", parts)
	}
}

// A budget alert at the patrol's urgent severity is the red the table gives a
// row in trouble, and one that arrives from an overdue patrol says on hover
// that the figure is that patrol's last report rather than this morning's.
func TestCostHeaderColoursTheBudgetByItsFinding(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"cloud": map[string]any{"state": "overdue", "findings": []any{budgetFinding("urgent")}},
	}))
	gcp := partByText(t, headerParts(t, dom), "GCP")
	if gcp.Class != "badc" {
		t.Errorf("an urgent budget alert wears %q, want the table's over-budget colour", gcp.Class)
	}
	if !strings.Contains(gcp.Title, "overdue") {
		t.Errorf("the overdue patrol's budget has tooltip %q, want it to say the patrol is overdue", gcp.Title)
	}
}

// A figure a row does not carry is left out of the header, never drawn as a
// zero: a provider that could not be read says nothing here, and a quiet town
// draws no line at all.
func TestCostHeaderOmitsFiguresItDoesNotHave(t *testing.T) {
	t.Parallel()

	// Rows that carry nothing: no figure to show, and no row to make one up for.
	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"cost_providers": []any{
			map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash"},
			map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "why": "the Claude plan has not been read yet"},
		},
	}))
	if note := dom.element(t, "costnote"); note.Text != "" || note.Kids != 0 {
		t.Errorf("a header of rows with no figures reads %q in %d children, want no line", note.Text, note.Kids)
	}

	// One row with one figure: the runway alone, with no "today $0.00" beside
	// it and the unreadable plan not drawn as 0%.
	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"cost_providers": []any{
			map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "runway_days": 7.74, "status": "ok"},
			map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "why": "the Claude plan has not been read yet"},
		},
	}))
	if note := dom.element(t, "costnote"); note.Text != "DeepSeek 7.7d runway" {
		t.Errorf("the header reads %q, want the runway alone", note.Text)
	}
}

// The ledger's time and the read's age answer "how old is this?", which the
// operator asks after the figures, so they are the header's tooltip; the beads
// over twenty minutes are a badge, and only when there are any.
func TestCostHeaderPutsTheAgesInItsTooltip(t *testing.T) {
	t.Parallel()

	usage := map[string]any{
		"as_of":  time.Now().UTC().Format(time.RFC3339),
		"window": "24h",
		"over20": []any{map[string]any{"bead": "gt-1"}, map[string]any{"bead": "gt-2"}},
	}
	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"cost_providers": []any{map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "today": 0.42}},
		"spend":          map[string]any{"balance": 77.49, "last_sample": "2026-10-30T11:19", "live": map[string]any{"ok": true}},
		"usage":          usage,
		"reads":          map[string]any{"spend": map[string]any{"at": time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339), "every_sec": 900}},
	}))
	note := dom.element(t, "costnote")
	if note.Text != "today $0.42 cash · 2 beads over 20m" {
		t.Errorf("the header reads %q, want today's figure and the badge only", note.Text)
	}
	for _, want := range []string{"ledger 2026-10-30 11:19", "read 4m ago"} {
		if !strings.Contains(note.Title, want) {
			t.Errorf("the header's tooltip = %q, want %q among it", note.Title, want)
		}
	}
	badge := partByText(t, headerParts(t, dom), "beads over 20m")
	if badge.Text != "2 beads over 20m" || !strings.Contains(badge.Class, "badge") {
		t.Errorf("the slow bead count drew as %+v, want a badge", badge)
	}
	if !strings.Contains(badge.Title, "twenty minutes") {
		t.Errorf("the badge's tooltip = %q, want it to say what it counts", badge.Title)
	}

	// One slow bead is a bead, not a beads.
	usage["over20"] = []any{map[string]any{"bead": "gt-1"}}
	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{"spend": liveSpend(), "usage": usage}))
	if note := dom.element(t, "costnote"); !strings.Contains(note.Text, "1 bead over 20m") {
		t.Errorf("the header with one slow bead reads %q", note.Text)
	}

	// No slow bead, no badge.
	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{"spend": liveSpend(), "usage": map[string]any{"as_of": usage["as_of"]}}))
	if note := dom.element(t, "costnote"); note.Text != "" {
		t.Errorf("a town with no slow bead drew a badge: %q", note.Text)
	}
}

// A spend read that is not current says so in the header, since grey figures
// alone must not pass as live ones: the header's line and its colour both say
// it, the way the page's other panes do.
func TestCostHeaderNamesAStaleSpendRead(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend":          liveSpend(),
		"cost_providers": []any{map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "today": 0.42}},
		"reads":          map[string]any{"spend": map[string]any{"stale": true, "error": true}},
	}))
	note := dom.element(t, "costnote")
	if !strings.Contains(note.Text, "read failed") {
		t.Errorf("the header of a failed read reads %q, want it to say the read failed", note.Text)
	}
	if !strings.Contains(note.Class, "badc") {
		t.Errorf("the header wears %q, want the page's failure colour", note.Class)
	}
	if got := partByText(t, headerParts(t, dom), "read failed"); got.Class != "badc" {
		t.Errorf("the read-failed part wears %q, want the failure colour", got.Class)
	}

	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend":          liveSpend(),
		"cost_providers": []any{map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "today": 0.42}},
		"reads":          map[string]any{"spend": map[string]any{"stale": true}},
	}))
	if note := dom.element(t, "costnote"); !strings.Contains(note.Class, "warnc") {
		t.Errorf("a stale read's header wears %q, want the amber colour", note.Class)
	}
	if got := partByText(t, headerParts(t, dom), "stale"); got.Class != "warnc" {
		t.Errorf("the stale part wears %q, want the amber colour", got.Class)
	}
}

// A spend report that is absent is named in the header, not left out: a line
// with no spend report behind it is not a quiet one.
func TestCostHeaderNamesAMissingSpendReport(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"cost_providers": []any{map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "today": 0.42}},
	}))
	if got := partByText(t, headerParts(t, dom), "no spend read"); got.Class != "warnc" {
		t.Errorf("a missing spend report reads %q, want the amber colour", got.Class)
	}
}
