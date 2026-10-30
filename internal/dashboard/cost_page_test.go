package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The Cost panel is one section where DeepSeek and Usage were two, with the GCP
// budget line beside them. There is no JS engine in the unit tier, so like the
// other page tests these pin the lines the page draws from, not a rendered DOM
// (gt-qqn0s).

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

// The budget line renders directly under the provider rows and above the spend
// part the daily chart folds into, so the alarm is read before the money.
func TestCostPanelDrawsGCPUnderTheProviderRows(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", map[string]any{
		"spend":          map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}},
		"usage":          map[string]any{"window": "24h"},
		"cloud":          map[string]any{"state": "fresh", "findings": []any{budgetFinding("urgent")}},
		"cost_providers": []any{map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "balance": 27.45}},
	})
	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children.map(c =>
		c.className === "spendpart" ? "spendpart"
			: c.textContent.indexOf("GCP budget") === 0 ? "gcp"
			: c.tagName === "TABLE" ? "providers" : "other"))`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	var order []string
	if err := json.Unmarshal([]byte(v.String()), &order); err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	if len(order) < 3 || strings.Join(order[:3], "|") != "providers|gcp|spendpart" {
		t.Errorf("panel order = %v, want the provider rows, then the budget line, then the spend part", order)
	}
}

// With neither a spend report nor usage there is one line naming them, under the
// providers line; the cloud's budget alert still shows, and the caption never
// says "undefined".
func TestCostPanelEmptyStateAndCaption(t *testing.T) {
	t.Parallel()

	// No spend command configured, usage not scanned yet, no providers: the
	// providers line and one honest line about the spend report. The server sends
	// an empty provider list, not a missing one, when there are none.
	lines, note := drawCost(t, map[string]any{"cost_providers": []any{}})
	if len(lines) != 2 || lines[0].Text != "no providers" || !strings.Contains(lines[1].Text, "no spend report") || !strings.Contains(lines[1].Text, "usage still reading") {
		t.Errorf("empty panel = %+v, want the no-providers line, then one line naming the spend report and the usage read", lines)
	}
	if note.Text != "" {
		t.Errorf("empty panel caption = %q, want none", note.Text)
	}

	// The same panel with a budget alert keeps the alert, directly under the
	// provider rows and above the line naming the spend report.
	lines, _ = drawCost(t, map[string]any{"cloud": map[string]any{"state": "fresh", "findings": []any{budgetFinding("urgent")}}})
	if len(lines) != 3 || !strings.HasPrefix(lines[1].Text, "GCP budget") || !strings.Contains(lines[2].Text, "no spend report") {
		t.Errorf("empty panel with an alert = %+v, want the providers line, the alert under it, then the spend report line", lines)
	}

	// Usage not yet answered beside a spend report: the report's ledger sample
	// names the reading in the header's tooltip, which is where the age of the
	// figures lives, and the line itself says no number it was not given.
	spend := map[string]any{"balance": 12.5, "last_sample": "2026-10-15T10:00:00", "live": map[string]any{"ok": true}}
	_, note = drawCost(t, map[string]any{"spend": spend})
	if strings.Contains(note.Text, "undefined") || strings.Contains(note.Text, "null") || strings.Contains(note.Title, "undefined") || !strings.Contains(note.Title, "ledger 2026-10-15 10:00:00") {
		t.Errorf("caption with usage absent = %q (tooltip %q)", note.Text, note.Title)
	}
	if note.Text != "" {
		t.Errorf("a spend report with no provider row drew a headline figure: %q", note.Text)
	}

	// Unreadable usage reaches the caption in the page's failure colour.
	_, note = drawCost(t, map[string]any{"spend": spend, "usage": map[string]any{"unavailable": true}})
	if !strings.Contains(note.Text, "usage unreadable") || !strings.Contains(note.Class, "badc") {
		t.Errorf("caption with unreadable usage = %q (%s)", note.Text, note.Class)
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
