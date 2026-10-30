package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// headerSep is the run of non-breaking spaces that sets the header's three
// groups apart, as the page draws it; the figures inside a group are joined by
// " · ".
const headerSep = "\u00a0\u00a0\u00a0"

// headerPart is one span of the Cost panel's header or expanded summary line as
// drawn: the words it shows, the class that colours them, and its tooltip.
type headerPart struct {
	Text  string `json:"text"`
	Class string `json:"class"`
	Title string `json:"title"`
}

// spanParts reads the element spans of the element js selects, in the order
// they were drawn. The page joins them with text nodes, which the stub keeps as
// children, so only elements are read back.
func spanParts(t *testing.T, dom *pageDOM, js string) []headerPart {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((` + js + `).children.filter(c => c.nodeType !== 3).map(c => ({text: c.textContent, class: c.className, title: c.title})))`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	var parts []headerPart
	if err := json.Unmarshal([]byte(v.String()), &parts); err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	return parts
}

// headerParts is the collapsed header's spans in the order it drew them.
func headerParts(t *testing.T, dom *pageDOM) []headerPart {
	t.Helper()
	return spanParts(t, dom, `__byId["costnote"]`)
}

// summaryLine is the expanded panel's line above the table: its words, and its
// spans. It is absent when there is nothing to say, and then the text is "".
func summaryLine(t *testing.T, dom *pageDOM) (string, []headerPart) {
	t.Helper()

	v, err := dom.vm.RunString(`(() => {
		const l = __byId["cost"].children.filter(c => c.className === "title")[0];
		return l ? JSON.stringify({text: l.textContent}) : "null";
	})()`)
	if err != nil {
		t.Fatalf("reading the Cost summary: %v", err)
	}
	if v.String() == "null" {
		return "", nil
	}
	var line struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(v.String()), &line); err != nil {
		t.Fatalf("reading the Cost summary: %v", err)
	}
	return line.Text, spanParts(t, dom, `__byId["cost"].children.filter(c => c.className === "title")[0]`)
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
	t.Fatalf("the line has no part saying %q: %+v", want, parts)
	return headerPart{}
}

// hasPartText reports whether some span says want.
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
	return map[string]any{"balance": 97.27, "live": map[string]any{"ok": true}}
}

// freshSpendRead is the spend read as the hub sends it when it was taken four
// minutes ago on a fifteen-minute poll: current, so the header says its age.
func freshSpendRead(extra map[string]any) map[string]any {
	read := map[string]any{"at": time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339), "every_sec": 900}
	for k, v := range extra {
		read[k] = v
	}
	return map[string]any{"spend": read}
}

// deepSeekRow and claudeRow are the provider rows the header draws from: the
// balance and the hourly spend, and the Claude week's percent and its hourly
// spend, with the limit's status as the row's own text.
func deepSeekRow() map[string]any {
	return map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "balance": 97.27, "per_hour_1h": 1.99, "runway_days": 7.74, "status": "ok", "status_text": "7.7d runway"}
}

func claudeRow() map[string]any {
	return map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "plan_pct": 44.0, "per_hour_1h": 0.85, "status": "ok", "status_text": "limit in 2.6d"}
}

// headerState is a reading whose rows carry every figure the collapsed header
// draws, and a current spend read.
func headerState(t *testing.T) map[string]any {
	t.Helper()

	return jsonValue(t, map[string]any{
		"spend":          liveSpend(),
		"reads":          freshSpendRead(nil),
		"cost_providers": []any{deepSeekRow(), claudeRow()},
	})
}

// The collapsed header is three things: DeepSeek's balance and its hourly spend,
// the Claude week's percent and its hourly spend, and the age of the spend read.
// The hourly figure is the table's Last 1h column.
func TestCostHeaderShowsTheCollapsedFigures(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, headerState(t))

	const want = "DeepSeek $97.27 · $1.99/h" + headerSep + "Claude 44% (wk) · $0.85/h" + headerSep + "read 4m ago"
	if note := dom.element(t, "costnote"); note.Text != want {
		t.Errorf("the header reads %q, want %q", note.Text, want)
	}

	parts := headerParts(t, dom)
	for text, class := range map[string]string{"DeepSeek": "good", "Claude": "good", "read": ""} {
		if got := partByText(t, parts, text).Class; got != class {
			t.Errorf("the %s part wears %q, want %q", text, got, class)
		}
	}

	// The limit's status is the row's text, on hover, and never in the line.
	if got := partByText(t, parts, "Claude").Title; got != "limit in 2.6d" {
		t.Errorf("the Claude part's tooltip = %q, want the row's status text", got)
	}
	for _, left := range []string{"today", "est.", "beads", "GCP", "ledger", "limit in", "runway"} {
		if note := dom.element(t, "costnote"); strings.Contains(note.Text, left) {
			t.Errorf("the header still says %q: %q; the expanded panel holds it", left, note.Text)
		}
	}
}

// A figure the row cannot give is drawn as "?": it is never left out of its part
// and never drawn as a zero. A provider the list does not name has no part at all,
// and a list that did not arrive leaves both figures unknown.
func TestCostHeaderDrawsAnUnreadableFigureAsAQuestionMark(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"reads": freshSpendRead(nil),
		"cost_providers": []any{
			map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "why": "the balance export failed"},
			map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "per_hour_1h": 0.85, "status": "ok"},
		},
	}))
	parts := headerParts(t, dom)
	if ds := partByText(t, parts, "DeepSeek"); ds.Text != "DeepSeek ? · ?" || ds.Title != "the balance export failed" {
		t.Errorf("an unreadable DeepSeek row reads %q (tooltip %q), want a ? for each figure and the reason on hover", ds.Text, ds.Title)
	}
	if cl := partByText(t, parts, "Claude"); cl.Text != "Claude ? · $0.85/h" {
		t.Errorf("a Claude row with no percent reads %q, want the percent as ?", cl.Text)
	}

	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend":          liveSpend(),
		"reads":          freshSpendRead(nil),
		"cost_providers": []any{claudeRow()},
	}))
	if hasPartText(headerParts(t, dom), "DeepSeek") {
		t.Errorf("a town with no DeepSeek row drew a DeepSeek part")
	}

	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{"spend": liveSpend(), "reads": freshSpendRead(nil)}))
	const want = "DeepSeek ? · ?" + headerSep + "Claude ? · ?" + headerSep + "read 4m ago"
	if note := dom.element(t, "costnote"); note.Text != want {
		t.Errorf("a provider list that did not arrive reads %q, want %q", note.Text, want)
	}
}

// A stale or failed spend read colours the header element the operator sees, and
// the parts the figures sit in, and its age part says so. A live read wears no
// warning or error colour at all. The classes are read from the rendered DOM.
func TestCostHeaderColoursAStaleOrFailedSpendRead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                   string
		read                   map[string]any
		noteClass, figureClass string
		ageText, ageClass      string
	}{
		{"live", nil, "r", "good", "read 4m ago", ""},
		{"stale", map[string]any{"stale": true}, "r warnc", "warnc", "read 4m ago · stale", "warnc"},
		{"failed", map[string]any{"stale": true, "error": true}, "r badc", "badc", "read failed", "badc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			drawCostOn(t, dom, jsonValue(t, map[string]any{
				"spend":          liveSpend(),
				"reads":          freshSpendRead(tc.read),
				"cost_providers": []any{deepSeekRow(), claudeRow()},
			}))
			if note := dom.element(t, "costnote"); note.Class != tc.noteClass {
				t.Errorf("the header element wears %q, want %q", note.Class, tc.noteClass)
			}
			parts := headerParts(t, dom)
			for _, text := range []string{"DeepSeek", "Claude"} {
				if got := partByText(t, parts, text).Class; got != tc.figureClass {
					t.Errorf("the %s part wears %q, want %q", text, got, tc.figureClass)
				}
			}
			if age := partByText(t, parts, "read"); age.Text != tc.ageText || age.Class != tc.ageClass {
				t.Errorf("the age part reads %q in %q, want %q in %q", age.Text, age.Class, tc.ageText, tc.ageClass)
			}
		})
	}
}

// A failed read names its reason in the line, from the DeepSeek row the failure
// left unreadable.
func TestCostHeaderNamesTheReasonAFailedReadLeftBehind(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": liveSpend(),
		"reads": freshSpendRead(map[string]any{"stale": true, "error": true}),
		"cost_providers": []any{
			map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "why": "the balance export failed"},
		},
	}))
	if got := partByText(t, headerParts(t, dom), "read failed"); got.Text != "read failed: the balance export failed" || got.Class != "badc" {
		t.Errorf("a failed read reads %q in %q, want the reason in the failure colour", got.Text, got.Class)
	}
}

// A spend report that is absent is named in the header, not left out: a line
// with no spend report behind it is not a quiet one.
func TestCostHeaderNamesAMissingSpendReport(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"cost_providers": []any{deepSeekRow()},
	}))
	if got := partByText(t, headerParts(t, dom), "no spend read"); got.Class != "warnc" {
		t.Errorf("a missing spend report reads %q, want the amber colour", got.Class)
	}
}

// The expanded panel's line above the table holds what the collapsed header let
// go, nothing deleted: today's total with cash and the plan's estimate apart, the
// DeepSeek estimate with its window on hover, the beads over twenty minutes as a
// badge, and the ledger's time.
func TestCostSummaryHoldsTheFiguresTheHeaderLeftOut(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend": map[string]any{"balance": 77.49, "last_sample": "2026-10-30T11:19", "live": map[string]any{"ok": true}},
		"cost_providers": []any{
			map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "today": 0.42},
			map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "today": 4.29, "plan_pct": 38.0, "status": "ok"},
		},
		"usage": map[string]any{
			"as_of":  time.Now().UTC().Format(time.RFC3339),
			"window": "24h",
			"by_class_model": []any{
				map[string]any{"model": "deepseek-chat", "est_cost_usd": 1.2},
				map[string]any{"model": "deepseek-reasoner", "est_cost_usd": 0.3},
				map[string]any{"model": "claude-opus", "est_cost_usd": 9.0},
			},
			"over20": []any{map[string]any{"bead": "gt-1"}, map[string]any{"bead": "gt-2"}},
		},
	}))
	text, parts := summaryLine(t, dom)
	const want = "today $0.42 cash · $4.29 plan est. · DeepSeek est. $1.50 · 2 beads over 20m · ledger 2026-10-30 11:19"
	if text != want {
		t.Errorf("the summary line reads %q, want %q", text, want)
	}
	if badge := partByText(t, parts, "beads over 20m"); badge.Text != "2 beads over 20m" || !strings.Contains(badge.Class, "badge") {
		t.Errorf("the slow bead count drew as %+v, want a badge", badge)
	}
	if est := partByText(t, parts, "DeepSeek est."); !strings.Contains(est.Title, "24h") {
		t.Errorf("the estimate's tooltip = %q, want its window", est.Title)
	}
	if note := dom.element(t, "costnote"); note.Title != "ledger 2026-10-30 11:19" {
		t.Errorf("the header's tooltip = %q, want the ledger's time", note.Title)
	}

	// Cash and the plan estimate are different money, so a plan-only day names
	// only the estimate; one slow bead is a bead, and no slow bead draws no badge.
	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{
		"spend":          liveSpend(),
		"cost_providers": []any{map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "today": 4.29, "plan_pct": 38.0, "status": "ok"}},
		"usage":          map[string]any{"as_of": time.Now().UTC().Format(time.RFC3339), "over20": []any{map[string]any{"bead": "gt-1"}}},
	}))
	if text, _ := summaryLine(t, dom); text != "today $4.29 plan est. · 1 bead over 20m" {
		t.Errorf("a plan-only day with one slow bead reads %q", text)
	}
	dom = newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{"spend": liveSpend(), "usage": map[string]any{"as_of": time.Now().UTC().Format(time.RFC3339)}}))
	if text, _ := summaryLine(t, dom); strings.Contains(text, "over 20m") {
		t.Errorf("a town with no slow bead drew a badge: %q", text)
	}
}

// An unreadable usage scan says so in the expanded panel, in the failure colour.
func TestCostSummaryNamesAnUnreadableUsageScan(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawCostOn(t, dom, jsonValue(t, map[string]any{"spend": liveSpend(), "usage": map[string]any{"unavailable": true}}))
	_, parts := summaryLine(t, dom)
	if got := partByText(t, parts, "usage unreadable"); got.Class != "badc" {
		t.Errorf("an unreadable usage scan wears %q, want the failure colour", got.Class)
	}
}
