package dashboard

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The Cost panel's usage blocks are folds of their own: each closed on load with
// one line on its heading, money and tokens in separate columns, one rework
// table with a window column, and the Deep reviews heading carries its state
// chip. These tests run the page's own renderCost over a reading and read back
// what it drew (gt-bocqt).

// costFoldState is one of the Cost panel's folds as the page keeps it: whether it
// is closed, the line its heading shows while closed, and what a redraw set in
// the heading beside its title.
type costFoldState struct {
	Closed  bool   `json:"closed"`
	Summary string `json:"summary"`
	Bits    string `json:"bits"`
}

// readCostFold reads back the fold keyed key, failing when the page keeps none.
func readCostFold(t *testing.T, dom *pageDOM, key string) costFoldState {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const f = folds.filter(x => x.key === ` + quoteJS(key) + `)[0];
		if (!f) return null;
		const sum = __find(__find(f.el, ":scope > h2"), ".foldsum");
		return {closed: f.closed, summary: sum ? sum.textContent : "", bits: costFolds[` + quoteJS(key) + `].bits.textContent};
	})())`)
	if err != nil {
		t.Fatalf("reading the %s fold: %v", key, err)
	}
	if v.String() == "null" {
		t.Fatalf("the page keeps no %s fold", key)
	}
	var out costFoldState
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s fold: %v", key, err)
	}
	return out
}

// foldCount is how many folds the page keeps under key, so a test can tell a fold
// that was not drawn from one drawn twice.
func foldCount(t *testing.T, dom *pageDOM, key string) int {
	t.Helper()

	v, err := dom.vm.RunString(`folds.filter(x => x.key === ` + quoteJS(key) + `).length`)
	if err != nil {
		t.Fatalf("counting the %s fold: %v", key, err)
	}
	return int(v.ToInteger())
}

// costFoldLines reads the lines a fold's body holds, each with its class and text.
func costFoldLines(t *testing.T, dom *pageDOM, key string) []costLine {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(costFolds[` + quoteJS(key) + `].body.children.map(c => ({class: c.className, text: c.textContent, title: c.title})))`)
	if err != nil {
		t.Fatalf("reading the %s fold: %v", key, err)
	}
	var lines []costLine
	if err := json.Unmarshal([]byte(v.String()), &lines); err != nil {
		t.Fatalf("reading the %s fold: %v", key, err)
	}
	return lines
}

// costFoldTables reads each table a fold's body holds as its rows of cell text.
func costFoldTables(t *testing.T, dom *pageDOM, key string) [][][]string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(costFolds[` + quoteJS(key) + `].body.children
		.filter(c => c.tagName === "TABLE")
		.map(tb => tb.children.map(r => r.children.map(x => x.textContent))))`)
	if err != nil {
		t.Fatalf("reading the %s tables: %v", key, err)
	}
	var tables [][][]string
	if err := json.Unmarshal([]byte(v.String()), &tables); err != nil {
		t.Fatalf("reading the %s tables: %v", key, err)
	}
	return tables
}

// usageReading is a Cost panel reading with every usage block drawn: a DeepSeek
// row and a Claude row, one deep review run four days old, two top beads, one
// bead over twenty minutes, the Jev read, and a scan of the records just now.
func usageReading(now time.Time) map[string]any {
	return map[string]any{
		"spend":          spendReading(),
		"cost_providers": []any{deepSeekProvider(), jevProvider()},
		"jev_spend": map[string]any{
			"calls": 12, "usd": 3.6, "window": "24h",
			"top_caller": "placeholder-caller", "top_caller_calls": 5, "top_caller_usd": 1.5,
		},
		"usage": map[string]any{
			"as_of":  now.UTC().Format(time.RFC3339),
			"window": "24h",
			"by_class_model": []any{
				map[string]any{"class": "interactive", "model": "deepseek-chat", "calls": 300, "uncached": 1000000, "est_cost_usd": 0.42},
				map[string]any{"class": "work", "model": "claude-opus", "calls": 34, "uncached": 200000, "est_cost_usd": 0},
			},
			"deep_review_runs": []any{
				map[string]any{"bead": "gt-dr1", "end": now.Add(-4 * 24 * time.Hour).UTC().Format(time.RFC3339), "wall_s": 600, "uncached": 90000, "subagent_share": 0.4},
			},
			"top_beads": []any{
				map[string]any{"bead": "gt-hwpfq", "rig": "gastown", "class": "work", "wall_s": 1800, "uncached": 433800, "est_cost_usd": 0.5},
				map[string]any{"bead": "gt-small", "rig": "gastown", "class": "work", "wall_s": 60, "uncached": 2000, "est_cost_usd": 0},
			},
			"over20":     []any{map[string]any{"bead": "gt-slow", "rig": "gastown", "wall_s": 1500, "test_runs": 2, "failed_test_runs": 0}},
			"unmatched":  3,
			"incomplete": 1,
			"skipped":    2,
		},
	}
}

// The six usage folds start closed, each with one line on its heading; the
// Cost panel no longer draws the native details block they replace.
func TestCostPanelFoldsStartClosedWithOneLineSummaries(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", usageReading(time.Now()))

	want := map[string]string{
		"cost.byclass":  "334 calls · 1.2M tokens",
		"cost.topbeads": "top gt-hwpfq 433.8k tokens",
		"cost.over20":   "1 bead in the last 48h",
		"cost.quality":  "3 unmatched · 1 incomplete · 2 skipped",
		"cost.jev":      "12 calls in 24h · top caller placeholder-caller (5 calls)",
	}
	for key, text := range want {
		got := readCostFold(t, dom, key)
		if !got.Closed {
			t.Errorf("%s starts open, want closed", key)
		}
		if got.Summary != text {
			t.Errorf("%s summary = %q, want %q", key, got.Summary, text)
		}
	}

	// Deep reviews names its run count and the age of the newest run, not a date.
	deep := readCostFold(t, dom, "cost.deepreview")
	if !deep.Closed || !strings.HasPrefix(deep.Summary, "1 run · last ") || !strings.HasSuffix(deep.Summary, " ago") {
		t.Errorf("cost.deepreview = closed %v, summary %q, want closed with one run and its age", deep.Closed, deep.Summary)
	}

	v, err := dom.vm.RunString(`(() => { let n = 0; __walk(__byId["cost"], c => { if (c.tagName === "DETAILS") n++; }); return n; })()`)
	if err != nil {
		t.Fatalf("looking for a details block: %v", err)
	}
	if details := int(v.ToInteger()); details != 0 {
		t.Errorf("the Cost panel still draws %d details block(s), want none", details)
	}
}

// By class and model and Top beads each draw their dollars and their tokens in
// separate columns: Spend ($) holds an estimate or a dash, Tokens holds the count
// of every row, and no cell holds both.
func TestCostUnitsSplitDollarsFromTokens(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", usageReading(time.Now()))

	spendCell := regexp.MustCompile(`^(est\. \$\d+\.\d{2}|–)$`)
	tokenCell := regexp.MustCompile(`^\d+(\.\d)?[kM]?$`)
	cases := []struct {
		key   string
		head  string
		width int
		rows  [][]string
	}{
		{"cost.byclass", "Class|Model|Calls|Spend ($)|Tokens", 5, [][]string{
			{"interactive", "deepseek-chat", "300", "est. $0.42", "1.0M"},
			{"work", "claude-opus", "34", "–", "200.0k"},
		}},
		{"cost.topbeads", "Bead|Class|Took|Spend ($)|Tokens", 5, [][]string{
			{"gt-hwpfq", "work", "30m", "est. $0.50", "433.8k"},
			{"gt-small", "work", "60s", "–", "2.0k"},
		}},
	}
	for _, tc := range cases {
		tables := costFoldTables(t, dom, tc.key)
		if len(tables) != 1 {
			t.Fatalf("%s drew %d tables, want one", tc.key, len(tables))
		}
		if head := strings.Join(tables[0][0], "|"); head != tc.head {
			t.Errorf("%s columns = %q, want %q", tc.key, head, tc.head)
		}
		rows := tables[0][1:]
		if len(rows) != len(tc.rows) {
			t.Fatalf("%s rows = %v, want %v", tc.key, rows, tc.rows)
		}
		for i, row := range rows {
			if len(row) != tc.width {
				t.Fatalf("%s row %d has %d cells, want %d: %v", tc.key, i, len(row), tc.width, row)
			}
			if strings.Join(row, "|") != strings.Join(tc.rows[i], "|") {
				t.Errorf("%s row %d = %v, want %v", tc.key, i, row, tc.rows[i])
			}
			if spend, tokens := row[3], row[4]; !spendCell.MatchString(spend) || !tokenCell.MatchString(tokens) {
				t.Errorf("%s row %d: spend %q and tokens %q are not one dollar cell and one token cell", tc.key, i, spend, tokens)
			}
		}
	}
}

// The rework table holds one row per window and bar, in the window's order, and
// keeps a bar that reviewed many times. The screenshot's data is three bars: the
// last seven days hold one and the seven days before them hold two, the one that
// saw a single review dropping out of the table.
func TestCostReworkDrawsOneRowPerWindowAndBar(t *testing.T) {
	t.Parallel()

	s := costSpendUsage()
	s["rework"] = map[string]any{"windows": []any{
		map[string]any{"name": "last 7 days", "bars": []any{
			map[string]any{"bar": 0.8, "reviews": 3, "rejected": 1, "beads": 2, "first_try": 1, "landed": 2, "reviews_per_landed": 1.5, "loops_out": 0, "mean_score": 0.8,
				"mean_om_secs": 190.0, "om_beads": 2, "median_om_secs": 190.0, "slowest_om_secs": 300.0, "slowest_om_bead": "gt-c"},
		}},
		map[string]any{"name": "the 7 days before", "bars": []any{
			map[string]any{"bar": 0.6, "reviews": 1, "rejected": 0, "beads": 1, "first_try": 1, "landed": 1, "reviews_per_landed": 1.0, "loops_out": 0, "mean_score": 0.6,
				"mean_om_secs": 60.0, "om_beads": 1, "median_om_secs": 60.0, "slowest_om_secs": 60.0, "slowest_om_bead": "gt-x"},
			map[string]any{"bar": 0.75, "reviews": 1046, "rejected": 500, "beads": 300, "first_try": 200, "landed": 250, "reviews_per_landed": 4.18, "loops_out": 2, "mean_score": 0.7,
				"mean_om_secs": 900.0, "om_beads": 250, "median_om_secs": 800.0, "slowest_om_secs": 3000.0, "slowest_om_bead": "gt-y"},
		}},
	}}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)
	tables := costFoldTables(t, dom, "cost.rework")
	if len(tables) != 1 {
		t.Fatalf("drew %d tables, want one for both windows", len(tables))
	}
	if head := strings.Join(tables[0][0], "|"); !strings.HasPrefix(head, "Window|Bar|Reviews|") {
		t.Errorf("columns = %q, want the Window column in front", head)
	}
	rows := tables[0][1:]
	want := [][]string{
		{"last 7 days", "0.80", "3"},
		{"the 7 days before", "0.75", "1046"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want one per window and bar with more than one review", rows)
	}
	for i := range want {
		if got := rows[i][:3]; strings.Join(got, "|") != strings.Join(want[i], "|") {
			t.Errorf("row %d = %v, want %v", i, got, want[i])
		}
	}
}

// The Deep reviews heading carries a paused chip while the schedule is off, a
// scheduled chip while it is on and has runs, an unknown chip while the state is
// missing or could not be read — a read that did not arrive is not a healthy
// schedule — and the date of the last run beside it whenever there is one.
func TestCostDeepReviewsChipIsPausedOrScheduled(t *testing.T) {
	t.Parallel()

	now := time.Now()
	run := []any{map[string]any{"bead": "gt-dr1", "end": now.Add(-4 * 24 * time.Hour).UTC().Format(time.RFC3339), "wall_s": 600, "uncached": 90000, "subagent_share": 0.4}}
	patrolOn := []any{map[string]any{"name": "deep-review-slice", "next": "in 3h"}}
	patrolOff := []any{map[string]any{"name": "deep-review-slice", "next": "patrol disabled"}}
	// The date is the page's locale date, which a browser words as "Oct 25" and the
	// test engine as "10/25/2026": either way a figure follows the words.
	dated := regexp.MustCompile(`last run \S.*\d`)

	cases := []struct {
		name       string
		deepreview map[string]any
		runs       []any
		chip       string
	}{
		{"the schedule's patrol is switched off", map[string]any{"schedules": patrolOff}, run, "paused"},
		{"the town holds dispatch", map[string]any{"schedules": patrolOn, "paused": "the town is held"}, run, "paused"},
		{"the schedule is on and has runs", map[string]any{"schedules": patrolOn}, run, "scheduled"},
		{"the schedule is on with no runs", map[string]any{"schedules": patrolOn}, []any{}, ""},
		{"the state names no schedule", map[string]any{}, run, ""},
		{"the schedule's read failed", map[string]any{"unavailable": true, "schedules": patrolOn}, run, "unknown"},
		{"the state never arrived", nil, run, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := usageReading(now)
			s["deepreview"] = tc.deepreview
			s["usage"].(map[string]any)["deep_review_runs"] = tc.runs
			dom := newPageDOM(t)
			dom.call(t, "renderCost", s)

			bits := readCostFold(t, dom, "cost.deepreview").Bits
			if tc.chip == "" {
				if strings.Contains(bits, "paused") || strings.Contains(bits, "scheduled") {
					t.Errorf("heading holds %q, want no state chip", bits)
				}
			} else if !strings.Contains(bits, tc.chip) {
				t.Errorf("heading holds %q, want the %s chip", bits, tc.chip)
			}
			// An unread state is never reported as a healthy schedule.
			if tc.chip == "unknown" && strings.Contains(bits, "scheduled") {
				t.Errorf("heading holds %q, want no scheduled chip while the state is unread", bits)
			}
			if len(tc.runs) == 0 {
				if strings.Contains(bits, "last run") {
					t.Errorf("heading holds %q with no run to date", bits)
				}
				return
			}
			if !dated.MatchString(bits) {
				t.Errorf("heading holds %q, want the last run's date beside the chip", bits)
			}
		})
	}
}

// The Data quality fold holds the three counts and the scan's age, stale and
// coloured when old; the Jev fold holds the calls, window and top caller, and
// only a configured Jev reader has a fold at all.
func TestCostPanelDataQualityAndJevFolds(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := usageReading(now)
	s["usage"].(map[string]any)["as_of"] = now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)

	var scanned, counts bool
	for _, l := range costFoldLines(t, dom, "cost.quality") {
		if l.Class == "empty warnc" && strings.HasPrefix(l.Text, "records last scanned ") && strings.HasSuffix(l.Text, "(stale)") {
			scanned = true
		}
		if l.Class == "empty" && l.Text == "3 unmatched · 1 incomplete · 2 skipped" {
			counts = true
		}
	}
	if !scanned || !counts {
		t.Errorf("data quality fold = %+v, want the stale scan age and the three counts", costFoldLines(t, dom, "cost.quality"))
	}

	jev := costFoldLines(t, dom, "cost.jev")
	if len(jev) != 1 || jev[0].Text != "12 calls in 24h · top caller placeholder-caller (5 calls)" {
		t.Errorf("jev fold = %+v, want the calls, window and top caller line", jev)
	}

	// A configured Jev reader whose read failed says so in its fold.
	failed := usageReading(now)
	delete(failed, "jev_spend")
	dom = newPageDOM(t)
	dom.call(t, "renderCost", failed)
	if lines := costFoldLines(t, dom, "cost.jev"); len(lines) != 1 || lines[0].Class != "badc" {
		t.Errorf("jev fold with a failed read = %+v, want one failure line", lines)
	}

	// A town with no Jev reader has neither a row nor a fold.
	unwired := usageReading(now)
	delete(unwired, "jev_spend")
	unwired["cost_providers"] = []any{deepSeekProvider()}
	dom = newPageDOM(t)
	dom.call(t, "renderCost", unwired)
	if n := foldCount(t, dom, "cost.jev"); n != 0 {
		t.Errorf("an unwired Jev reader drew %d jev folds, want none", n)
	}
}

// The Jev fold is the call line; the dollars ride on the Jev provider row of the
// table above it, from the same read, so the fold needs no money column of its
// own.
func TestCostJevFoldLeavesTheDollarsToTheProviderRow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", usageReading(time.Now()))

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const t = __byId["cost"].children.filter(c => c.className === "costprov")[0];
		if (!t) return null;
		return t.children.slice(1).map(r => r.children.map(c => c.textContent)).filter(r => r[0] === "Jev");
	})())`)
	if err != nil {
		t.Fatalf("reading the provider rows: %v", err)
	}
	if v.String() == "null" {
		t.Fatal("the Cost panel drew no provider table")
	}
	var rows [][]string
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the provider rows: %v", err)
	}
	// Provider, Kind, Balance or plan, Today, Yesterday, 7-day avg, Last 1h, Last
	// 24h, Status: the Jev row's Today is its dollars spent.
	if len(rows) != 1 {
		t.Fatalf("provider rows = %v, want one Jev row", rows)
	}
	if got := rows[0][3]; got != "$0.50" {
		t.Errorf("the Jev row's Today = %q, want its dollars", got)
	}

	// The fold itself carries calls and the top caller, and no dollar figure.
	for _, l := range costFoldLines(t, dom, "cost.jev") {
		if strings.Contains(l.Text, "$") {
			t.Errorf("jev fold line %q holds a dollar figure, want the dollars on the provider row", l.Text)
		}
	}
}

// A window with no usage, no top beads, no slow beads and no deep review runs
// says so in each fold, and a scan that never ran says so instead of an age.
func TestCostPanelFoldsNameTheirEmptyStates(t *testing.T) {
	t.Parallel()

	s := map[string]any{
		"spend":          spendReading(),
		"cost_providers": []any{},
		"deepreview":     map[string]any{"schedules": []any{}},
		"usage": map[string]any{
			"window":           "24h",
			"by_class_model":   []any{},
			"deep_review_runs": []any{},
			"top_beads":        []any{},
			"over20":           []any{},
		},
	}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", s)

	want := map[string]string{
		"cost.byclass":    "no usage in the last 24h",
		"cost.deepreview": "no deep review runs",
		"cost.topbeads":   "no beads in the last 24h",
		"cost.over20":     "none in the last 48h",
	}
	for key, text := range want {
		lines := costFoldLines(t, dom, key)
		if len(lines) != 1 || lines[0].Text != text {
			t.Errorf("%s = %+v, want the empty line %q", key, lines, text)
		}
	}
	quality := costFoldLines(t, dom, "cost.quality")
	if len(quality) != 2 || quality[0].Class != "empty warnc" || quality[0].Text != "never scanned" || quality[1].Text != "0 unmatched · 0 incomplete · 0 skipped" {
		t.Errorf("data quality fold = %+v, want the never-scanned warning and the counts", quality)
	}
}
