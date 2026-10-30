package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Cost panel draws its providers from the cost_providers array the server
// serves: one table, one row per provider, the same nine columns for each. These
// tests run the page's own renderCost over a reading and read back what it drew,
// so a row the page would not draw fails here, not in a review.

// costTableRow is one row of the providers table as the page drew it: its class,
// its hover title, and each cell's text, class and the chip inside it, if any.
type costTableRow struct {
	Class string          `json:"class"`
	Title string          `json:"title"`
	Cells []costTableCell `json:"cells"`
}

type costTableCell struct {
	Text  string `json:"text"`
	Class string `json:"class"`
	Chip  string `json:"chip"`
}

// costElem is one element a Cost fold or the panel drew: its tag, its class and
// its text.
type costElem struct {
	Tag   string `json:"tag"`
	Class string `json:"class"`
	Text  string `json:"text"`
}

// costTable reads back the providers table the Cost panel drew: the header row,
// then one row per provider. A panel that drew no table fails the test.
func costTable(t *testing.T, dom *pageDOM) []costTableRow {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const tbl = __byId["cost"].children.filter(c => c.className === "costprov")[0];
		if (!tbl) return null;
		return tbl.children.map(tr => ({
			class: tr.className, title: tr.title || "",
			cells: tr.children.map(td => ({text: td.textContent, class: td.className, chip: td.children.length ? td.children[0].className : ""}))
		}));
	})())`)
	if err != nil {
		t.Fatalf("reading the providers table: %v", err)
	}
	if v.String() == "null" {
		t.Fatal("the Cost panel drew no providers table")
	}
	var rows []costTableRow
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the providers table: %v", err)
	}
	return rows
}

// panelTexts reads the text of each element of the Cost panel that carries class,
// at the panel's level or in its spend part: the warnings under the table and
// the no-providers line sit at the panel's level, the no-spend-report line in the
// spend part.
func panelTexts(t *testing.T, dom *pageDOM, class string) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const cost = __byId["cost"];
		const spend = cost.children.filter(c => c.className === "spendpart")[0];
		return cost.children.concat(spend ? spend.children : []).filter(c => c.className === ` + quoteJS(class) + `).map(c => c.textContent);
	})())`)
	if err != nil {
		t.Fatalf("reading the Cost panel for %q: %v", class, err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the Cost panel for %q: %v", class, err)
	}
	return out
}

// costFoldChildren reads the children of the body of the chart fold keyed key,
// as the page drew them under the Cost panel's spend part. A key the page did not
// draw fails the test, since a fold that is missing is what the test is for.
func costFoldChildren(t *testing.T, dom *pageDOM, key string) []costElem {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const sec = __byId["cost"].children.filter(c => c.className === "spendpart")[0].children.filter(c => c.dataset && c.dataset.fold === ` + quoteJS(key) + `)[0];
		if (!sec) return null;
		return sec.children[1].children.map(c => ({tag: c.tagName, class: c.className || c.getAttribute("class") || "", text: c.textContent}));
	})())`)
	if err != nil {
		t.Fatalf("reading the %s fold: %v", key, err)
	}
	if v.String() == "null" {
		t.Fatalf("the Cost panel drew no %s fold", key)
	}
	var out []costElem
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s fold: %v", key, err)
	}
	return out
}

// costTableDrawn reports whether the Cost panel drew a providers table at all.
func costTableDrawn(t *testing.T, dom *pageDOM) bool {
	t.Helper()

	v, err := dom.vm.RunString(`__byId["cost"].children.some(c => c.className === "costprov")`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	return v.ToBoolean()
}

// costLegendEntries reads the labels of the legend the fold keyed key draws, one
// per series, in the order it lists them.
func costLegendEntries(t *testing.T, dom *pageDOM, key string) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const sec = __byId["cost"].children.filter(c => c.className === "spendpart")[0].children.filter(c => c.dataset && c.dataset.fold === ` + quoteJS(key) + `)[0];
		const lg = sec.children[1].children.filter(c => c.className === "legend")[0];
		return lg ? lg.children.map(x => x.textContent) : [];
	})())`)
	if err != nil {
		t.Fatalf("reading the %s legend: %v", key, err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s legend: %v", key, err)
	}
	return out
}

// deepSeekProvider is the DeepSeek row as the server sends it: cash, its balance
// and days, its rates, its runway as the status, and the hover lines.
func deepSeekProvider() map[string]any {
	return map[string]any{
		"id": "deepseek", "label": "DeepSeek", "kind": "cash",
		"balance": 27.45, "today": 1.2, "yesterday": 3.4, "avg_7d": 2.5,
		"per_hour_1h": 0.05, "per_hour_24h": 0.1, "runway_days": 3.0,
		"status": "warn", "status_text": "3.0d runway",
		"detail": []string{"DeepSeek $10.00 cash + $17.45 credit · credit lasts ~4.0d, then cash", "2026-10-28 +$5.00 top-up", "ledger 2026-10-29 06:00"},
	}
}

// claudeProvider is the Claude plan row: a plan kind, its share of the week, and
// the headline's day and rate figures.
func claudeProvider() map[string]any {
	return map[string]any{
		"id": "claude", "label": "Claude", "kind": "plan",
		"plan_pct": 6.0, "today": 0.8, "yesterday": 1.1, "avg_7d": 0.9,
		"per_hour_1h": 0.02, "per_hour_24h": 0.03,
		"status": "ok", "status_text": "limit in 5.7d",
		"detail": []string{"plan pace last 24h $0.03/h, approximate"},
	}
}

// jevProvider is the Jev row, a cash row with no balance and no yesterday.
func jevProvider() map[string]any {
	return map[string]any{
		"id": "jev", "label": "Jev", "kind": "cash",
		"today": 0.5, "per_hour_24h": 0.02,
		"status": "none",
		"detail": []string{"3 calls in 24h", "top caller ops (2 calls, $0.40)"},
	}
}

// costPanelState is the reading the Cost panel draws from, with the providers
// the server sent and whatever else the test sets on it.
func costPanelState(providers []map[string]any, extra map[string]any) map[string]any {
	list := make([]any, 0, len(providers))
	for _, p := range providers {
		list = append(list, p)
	}
	s := map[string]any{"cost_providers": list}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

// spendReading is a spend report with the fields the Cost panel's charts and
// its no-report line read; the table's figures come from cost_providers.
func spendReading() map[string]any {
	return map[string]any{"balance": 27.45, "live": map[string]any{"ok": true}, "watcher_pid": 42}
}

// costChartFixture is a chart as the server builds it: one bar per bucket, and
// the optional Claude series and limit line.
func costChartFixture(buckets int, claude bool, limit bool) map[string]any {
	bars := make([]any, 0, buckets)
	series := make([]any, 0, buckets)
	for i := 0; i < buckets; i++ {
		label := strconv.Itoa(i)
		bars = append(bars, map[string]any{"label": label, "value": float64(i + 1), "title": "bucket " + label})
		series = append(series, map[string]any{"label": label, "value": 0.5, "title": "claude " + label})
	}
	c := map[string]any{"bars": bars, "aria": "spend chart"}
	if claude {
		c["claude"] = map[string]any{"name": "Claude, plan-equivalent estimate", "bars": series}
	}
	if limit {
		c["limit"] = map[string]any{"value": 5.0, "label": "weekly limit $5.00/day · reset at 110%"}
	}
	return c
}

// The table draws one row per provider in the array's order, under a header of
// the nine columns, and each figure is the server's own, formatted as the panel
// formats money.
func TestCostProvidersTableDrawsOneRowPerProviderWithNineColumns(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claudeProvider(), jevProvider()}, map[string]any{"spend": spendReading()}))
	rows := costTable(t, dom)
	if len(rows) != 4 {
		t.Fatalf("the table has %d rows, want a header and three providers", len(rows))
	}

	wantHead := []string{"Provider", "Kind", "Balance or plan", "Today", "Yesterday", "7-day avg", "Last 1h ($/h)", "Last 24h ($/h)", "Status"}
	if got := cellTexts(rows[0]); strings.Join(got, "|") != strings.Join(wantHead, "|") {
		t.Errorf("the header is %q, want %q", got, wantHead)
	}
	wantRows := [][]string{
		{"DeepSeek", "cash", "$27.45", "$1.20", "$3.40", "$2.50", "$0.05", "$0.10", "3.0d runway"},
		{"Claude", "plan", "6% (wk)", "$0.80", "$1.10", "$0.90", "$0.02", "$0.03", "limit in 5.7d"},
		{"Jev", "cash", "–", "$0.50", "–", "–", "–", "$0.02", "–"},
	}
	for i, want := range wantRows {
		if got := cellTexts(rows[i+1]); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("row %d draws %q, want %q", i+1, got, want)
		}
	}
	if chip := rows[1].Cells[8].Chip; chip != "costchip warnc" {
		t.Errorf("DeepSeek's status chip is %q, want the amber chip", chip)
	}
}

// Every provider's row carries the same nine cells, so the table has one shape:
// a provider the page never names draws the same as one it does.
func TestCostProvidersTableDrawsAFourthProviderWithNoPageChange(t *testing.T) {
	t.Parallel()

	gcp := map[string]any{"id": "gcp", "label": "GCP", "kind": "cash", "balance": 5.0, "status": "ok", "status_text": "budget ok"}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claudeProvider(), jevProvider(), gcp}, map[string]any{"spend": spendReading()}))
	rows := costTable(t, dom)
	if len(rows) != 5 {
		t.Fatalf("the table has %d rows, want a header and four providers", len(rows))
	}
	row := rows[4]
	if len(row.Cells) != 9 {
		t.Fatalf("the fourth provider's row has %d cells, want 9", len(row.Cells))
	}
	if got := cellTexts(row); got[0] != "GCP" || got[2] != "$5.00" || got[8] != "budget ok" {
		t.Errorf("the fourth provider draws %q, want its label, balance and status", got)
	}
	if row.Cells[8].Chip != "costchip good" {
		t.Errorf("the fourth provider's chip is %q, want the green chip", row.Cells[8].Chip)
	}
}

// A provider that could not be read keeps its row, dim, with its figures as
// dashes and the reason in the status cell. A null figure is a dash on any row.
func TestCostProvidersTableDrawsANullAsADashAndAWhyRowDim(t *testing.T) {
	t.Parallel()

	claude := map[string]any{"id": "claude", "label": "Claude", "kind": "plan", "why": "the Claude plan has not been read yet", "status": "none"}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claude}, map[string]any{"spend": spendReading()}))
	rows := costTable(t, dom)
	row := rows[2]
	if row.Class != "dimrow" {
		t.Errorf("the unread provider's row is %q, want the dim row", row.Class)
	}
	for i, c := range row.Cells[2:8] {
		if c.Text != "–" {
			t.Errorf("the unread provider's cell %d draws %q, want a dash", i+2, c.Text)
		}
	}
	if status := row.Cells[8]; status.Text != "the Claude plan has not been read yet" {
		t.Errorf("the unread provider's status cell draws %q, want the reason", status.Text)
	}
	if rows[1].Class == "dimrow" {
		t.Error("a provider that was read is drawn dim")
	}
}

// The status chip takes its colour from the server's status: green, amber or red,
// and a provider with nothing to judge draws its status text in the dim chip, or
// a dash when it has no status text at all.
func TestCostProvidersTableColoursTheStatusChip(t *testing.T) {
	t.Parallel()

	row := func(id, status, text string) map[string]any {
		return map[string]any{"id": id, "label": id, "kind": "cash", "status": status, "status_text": text}
	}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{
		row("ok", "ok", "fine"), row("warn", "warn", "soon"), row("bad", "bad", "out"),
		row("none", "none", "nothing to judge"), row("bare", "none", ""),
	}, nil))
	rows := costTable(t, dom)
	want := map[int]string{1: "costchip good", 2: "costchip warnc", 3: "costchip badc", 4: "costchip"}
	for i, chip := range want {
		if got := rows[i].Cells[8].Chip; got != chip {
			t.Errorf("row %d chip is %q, want %q", i, got, chip)
		}
	}
	if bare := rows[5].Cells[8]; bare.Text != "–" || bare.Chip != "" {
		t.Errorf("a provider with no status text draws %q, want a dash with no chip", bare.Text)
	}
}

// A row's detail lines are its hover, and a provider's warnings stay visible
// under the table, whatever its figures say.
func TestCostProvidersTableKeepsDetailInTheHoverAndWarningsVisible(t *testing.T) {
	t.Parallel()

	ds := deepSeekProvider()
	ds["warnings"] = []string{"live balance unreachable", "spend watcher is not running"}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{ds}, map[string]any{"spend": spendReading()}))
	rows := costTable(t, dom)
	if want := "DeepSeek $10.00 cash + $17.45 credit · credit lasts ~4.0d, then cash · 2026-10-28 +$5.00 top-up · ledger 2026-10-29 06:00"; rows[1].Title != want {
		t.Errorf("the row's hover is %q, want %q", rows[1].Title, want)
	}
	warns := panelTexts(t, dom, "badc")
	for _, w := range []string{"DeepSeek · live balance unreachable", "DeepSeek · spend watcher is not running"} {
		if !containsText(warns, w) {
			t.Errorf("the warning %q is not drawn under the table; the panel drew %q", w, warns)
		}
	}
}

// With no spend report the table still draws, since its rows come from the other
// readers too, and the panel says the report is missing. The charts need the
// report's readings, so no fold is drawn.
func TestCostProvidersTableDrawsWithoutASpendReport(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{claudeProvider(), jevProvider()}, map[string]any{
		"cost_charts": map[string]any{"daily": costChartFixture(3, false, false)},
	}))
	rows := costTable(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the table has %d rows with no spend report, want a header and two providers", len(rows))
	}
	if got := panelTexts(t, dom, "empty"); !containsText(got, "no spend report; usage still reading…") {
		t.Errorf("the panel does not say the spend report is missing; it drew %q", got)
	}
	if f := costFoldKeys(t, dom); len(f) != 0 {
		t.Errorf("the charts drew folds %q with no spend report and no usage", f)
	}
}

// The server always sends cost_providers, so a reading with no key did not arrive:
// it draws no table and says the provider list is unavailable, not that there are
// none. A null is the same reading as a missing key.
func TestCostProvidersTableSaysUnavailableWhenTheKeyIsAbsent(t *testing.T) {
	t.Parallel()

	for name, reading := range map[string]map[string]any{
		"absent": {"spend": spendReading()},
		"null":   {"spend": spendReading(), "cost_providers": nil},
	} {
		dom := newPageDOM(t)
		dom.call(t, "renderCost", reading)
		if costTableDrawn(t, dom) {
			t.Errorf("%s: the panel drew a providers table from a reading with no list", name)
		}
		if got := panelTexts(t, dom, "empty warnc"); !containsText(got, "unavailable — the reading has no provider list") {
			t.Errorf("%s: the panel does not say the provider list is unavailable; it drew %q", name, got)
		}
		if got := panelTexts(t, dom, "empty"); containsText(got, "no providers") {
			t.Errorf("%s: the panel says there are no providers for a list that did not arrive; it drew %q", name, got)
		}
	}
}

// A provider list that is not a list is a failed reading, and the panel says so
// rather than drawing nothing.
func TestCostProvidersTableSaysSoForAReadingThatIsNotAList(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", map[string]any{"spend": spendReading(), "cost_providers": "garbled"})
	if costTableDrawn(t, dom) {
		t.Error("the panel drew a providers table from a value that is not a list")
	}
	if got := panelTexts(t, dom, "empty warnc"); !containsText(got, "unavailable — the reading has no provider list") {
		t.Errorf("the panel does not say the provider list failed; it drew %q", got)
	}
}

// An empty provider list draws no table and says there are no providers.
func TestCostProvidersTableSaysNoProvidersForAnEmptyList(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState(nil, map[string]any{"spend": spendReading()}))
	if got := panelTexts(t, dom, "empty"); !containsText(got, "no providers") {
		t.Errorf("the panel does not say there are no providers; it drew %q", got)
	}
}

// A zero 7-day average is a figure, drawn as $0.00, not a dash for a missing one.
func TestCostProvidersTableDrawsAZeroSevenDayAverageAsAFigure(t *testing.T) {
	t.Parallel()

	ds := deepSeekProvider()
	ds["avg_7d"] = 0.0
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{ds}, map[string]any{"spend": spendReading()}))
	rows := costTable(t, dom)
	if got := rows[1].Cells[5].Text; got != "$0.00" {
		t.Errorf("the 7-day average of 0 draws %q, want $0.00", got)
	}
}

// A stale spend read greys the DeepSeek row of the table, whose balance and rates
// are the spend read's, and the Claude and Jev rows stay clear: they are read
// elsewhere. The charts sit in the greyed spend part; the table is not in it.
func TestCostProvidersTableGreysOnlyTheDeepSeekRowForAStaleSpendRead(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claudeProvider(), jevProvider()}, map[string]any{
		"spend": spendReading(),
		"reads": map[string]any{"spend": map[string]any{"stale": true}},
	}))
	rows := costTable(t, dom)
	if len(rows) != 4 {
		t.Fatalf("the table drew %d rows, want the header and three providers", len(rows))
	}
	for _, c := range []struct {
		name string
		row  costTableRow
		grey bool
	}{{"DeepSeek", rows[1], true}, {"Claude", rows[2], false}, {"Jev", rows[3], false}} {
		if got := strings.Contains(c.row.Class, "jvstale"); got != c.grey {
			t.Errorf("%s row greyed = %v for a stale spend read, want %v (row class %q)", c.name, got, c.grey, c.row.Class)
		}
	}
	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const cost = __byId["cost"];
		const spend = cost.children.filter(c => c.className.split(" ").indexOf("spendpart") >= 0)[0];
		return {
			tableOnPanel: cost.children.some(c => c.className === "costprov"),
			spendGreyed: spend.className.split(" ").indexOf("stale") >= 0,
			tableInSpend: spend.children.some(c => c.className === "costprov")
		};
	})())`)
	if err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	var got struct {
		TableOnPanel bool `json:"tableOnPanel"`
		SpendGreyed  bool `json:"spendGreyed"`
		TableInSpend bool `json:"tableInSpend"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the Cost panel: %v", err)
	}
	if !got.SpendGreyed {
		t.Error("a stale spend read does not grey the spend part the charts are drawn in")
	}
	if !got.TableOnPanel || got.TableInSpend {
		t.Errorf("the providers table is on the panel %v and in the greyed part %v, want on the panel and not greyed", got.TableOnPanel, got.TableInSpend)
	}
}

// A fresh spend read greys no row: the DeepSeek figures are current.
func TestCostProvidersTableGreysNoRowForAFreshSpendRead(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claudeProvider()}, map[string]any{
		"spend": spendReading(),
		"reads": map[string]any{"spend": map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "every_sec": 900}},
	}))
	for _, row := range costTable(t, dom)[1:] {
		if strings.Contains(row.Class, "jvstale") {
			t.Errorf("a fresh spend read greys the row %q", row.Cells[0].Text)
		}
	}
}

// A failed Jev read keeps the Jev row, dim, with the reason in its status cell and
// its figures as dashes, so the failure reads as a failure in the table.
func TestCostProvidersTableDrawsAFailedJevReadAsADimRowWithItsReason(t *testing.T) {
	t.Parallel()

	jev := jevProvider()
	jev["today"] = nil
	jev["per_hour_24h"] = nil
	jev["why"] = "the Jev spend could not be read"
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), jev}, map[string]any{"spend": spendReading()}))
	rows := costTable(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the table drew %d rows, want the header and two providers", len(rows))
	}
	row := rows[2]
	if got := row.Cells[0].Text; got != "Jev" {
		t.Fatalf("the second provider row is %q, want Jev", got)
	}
	if !strings.Contains(row.Class, "dimrow") {
		t.Errorf("the failed Jev row class is %q, want dim", row.Class)
	}
	if got := row.Cells[8].Text; got != "the Jev spend could not be read" {
		t.Errorf("the failed Jev row's status is %q, want the reason", got)
	}
	for _, c := range row.Cells[2:8] {
		if c.Text != "–" {
			t.Errorf("a figure of the failed Jev row draws %q, want a dash", c.Text)
		}
	}
}

// The failed-read row is drawn from the value the SERVER builds, not from a
// fixture written to look like it: BuildCostProviders is run with no Jev reading,
// its array goes through JSON as the page receives it, and the page's own
// renderCost draws it. The reason the row shows is the one the Go side set, so a
// renamed field, a dropped omitempty or a changed reason fails here (gt-p39fq).
func TestCostProvidersTableDrawsTheServersFailedJevRead(t *testing.T) {
	t.Parallel()

	built := BuildCostProviders(time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC), json.RawMessage(`null`), nil, nil, nil)
	var jevWhy string
	for _, p := range built {
		if p.ID == costProviderJev {
			jevWhy = p.Why
		}
	}
	if jevWhy == "" {
		t.Fatal("the server built no Jev row with a reason for a missing Jev reading")
	}
	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var providers []map[string]any
	if err := json.Unmarshal(raw, &providers); err != nil {
		t.Fatal(err)
	}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState(providers, map[string]any{"spend": spendReading()}))
	var jev *costTableRow
	rows := costTable(t, dom)
	for i := range rows {
		if len(rows[i].Cells) > 0 && rows[i].Cells[0].Text == "Jev" {
			jev = &rows[i]
		}
	}
	if jev == nil {
		t.Fatalf("the table drew no Jev row from the server's array (%d rows)", len(rows))
	}
	if !strings.Contains(jev.Class, "dimrow") {
		t.Errorf("the server's failed Jev row class is %q, want dim", jev.Class)
	}
	if got := jev.Cells[8].Text; got != jevWhy {
		t.Errorf("the failed Jev row shows %q, want the server's reason %q", got, jevWhy)
	}
	for _, c := range jev.Cells[2:8] {
		if c.Text != "–" {
			t.Errorf("a figure of the server's failed Jev row draws %q, want a dash", c.Text)
		}
	}
}

// Each legend swatch carries the colour of the bar it names, whatever order the
// providers come in: DeepSeek's blue and Claude's violet, not their places in
// the array.
func TestCostLegendSwatchesMatchTheirBars(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{claudeProvider(), deepSeekProvider()}, map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"daily": costChartFixture(3, true, false)},
	}))
	got := costLegendSwatches(t, dom, "cost.daily")
	want := []string{"var(--violet)", "var(--blue)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the swatches are %q, want %q (Claude then DeepSeek, in the array's order)", got, want)
	}
}

// A chart's legend survives a reading with no provider list: its series are
// still drawn, so they are still named.
func TestCostLegendNamesItsSeriesWithoutTheArray(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"daily": costChartFixture(3, true, false)},
	})
	want := []string{"DeepSeek", "Claude, plan-equivalent estimate"}
	if got := costLegendEntries(t, dom, "cost.daily"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the legend lists %q with no provider list, want %q", got, want)
	}
}

// costLegendSwatches reads the colour of each swatch in the legend the fold keyed
// key draws, in the order the entries list.
func costLegendSwatches(t *testing.T, dom *pageDOM, key string) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const sec = __byId["cost"].children.filter(c => c.className === "spendpart")[0].children.filter(c => c.dataset && c.dataset.fold === ` + quoteJS(key) + `)[0];
		const lg = sec.children[1].children.filter(c => c.className === "legend")[0];
		return lg ? lg.children.map(x => x.children[0].style.background) : [];
	})())`)
	if err != nil {
		t.Fatalf("reading the %s swatches: %v", key, err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s swatches: %v", key, err)
	}
	return out
}

// The daily and hourly spend charts are folds under their own headings: the
// daily one starts open and the hourly one closed, and each folds on its own key.
func TestCostChartFoldsDailyOpenHourlyClosedEachSavedUnderItsKey(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider()}, map[string]any{
		"spend": spendReading(),
		"cost_charts": map[string]any{
			"daily":  costChartFixture(3, false, true),
			"hourly": costChartFixture(4, false, false),
		},
	}))
	if got := foldClosed(t, dom, "cost.daily"); got {
		t.Error("Daily spend starts closed; it starts open")
	}
	if got := foldClosed(t, dom, "cost.hourly"); !got {
		t.Error("Hourly spend starts open; it starts closed")
	}

	// A click on the hourly heading opens it, and the choice is saved under its
	// own key and not the daily one's.
	if _, err := dom.vm.RunString(`(() => { const sec = folds.filter(f => f.key === "cost.hourly")[0].el; __fireOn(sec.children[0], "click", {target: sec.children[0]}); })()`); err != nil {
		t.Fatalf("clicking Hourly spend: %v", err)
	}
	if got := foldClosed(t, dom, "cost.hourly"); got {
		t.Error("Hourly spend did not open on a click")
	}
	saved := dom.storage(t)["gt-fold"]
	if !strings.Contains(saved, `"cost.hourly":false`) || strings.Contains(saved, "cost.daily") {
		t.Errorf("the choice is saved as %q, want the hourly key alone", saved)
	}
}

// A redraw reuses the fold it drew before, so a reading that arrives every few
// seconds does not stack a second fold on the same key.
func TestCostChartFoldIsBuiltOnceAcrossRedraws(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	s := costPanelState([]map[string]any{deepSeekProvider()}, map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"daily": costChartFixture(3, false, false)},
	})
	dom.call(t, "renderCost", s)
	dom.call(t, "renderCost", s)
	v, err := dom.vm.RunString(`folds.filter(f => f.key === "cost.daily").length`)
	if err != nil {
		t.Fatalf("counting the daily fold: %v", err)
	}
	if n := v.ToInteger(); n != 1 {
		t.Errorf("the daily chart has %d folds after two redraws, want 1", n)
	}
}

// The legend lists exactly the series the chart draws, one per provider that
// has one, in the providers' order: a provider with no series has no entry, and
// a fourth provider adds none.
func TestCostLegendListsOnlyTheSeriesDrawn(t *testing.T) {
	t.Parallel()

	gcp := map[string]any{"id": "gcp", "label": "GCP", "kind": "cash", "status": "none"}
	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claudeProvider(), jevProvider(), gcp}, map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"daily": costChartFixture(3, true, false)},
	}))
	want := []string{"DeepSeek", "Claude, plan-equivalent estimate"}
	if got := costLegendEntries(t, dom, "cost.daily"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the legend lists %q, want %q", got, want)
	}

	// A Claude series the server sent without a name is labelled with its
	// provider's own label, not dropped from the legend.
	unnamed := costChartFixture(3, false, false)
	unnamed["claude"] = map[string]any{"bars": unnamed["bars"]}
	dom = newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claudeProvider()}, map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"daily": unnamed},
	}))
	if got := costLegendEntries(t, dom, "cost.daily"); strings.Join(got, "|") != "DeepSeek|Claude" {
		t.Errorf("the legend lists %q for an unnamed Claude series, want DeepSeek and Claude", got)
	}

	// Without a Claude series the chart draws the DeepSeek bars alone, and the
	// legend says so: no Claude entry.
	dom = newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider(), claudeProvider()}, map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"daily": costChartFixture(3, false, false)},
	}))
	if got := costLegendEntries(t, dom, "cost.daily"); strings.Join(got, "|") != "DeepSeek" {
		t.Errorf("the legend lists %q with no Claude series, want DeepSeek alone", got)
	}
}

// The daily limit is a dashed line, and its words are a caption under the legend,
// not text over the bars: no limit label is drawn inside the chart.
func TestCostDailyChartLimitIsALineWithACaptionNotText(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider()}, map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"daily": costChartFixture(3, false, true)},
	}))
	for _, c := range costSVGChildren(t, dom, "cost.daily") {
		if c.Class == "limitlbl" {
			t.Errorf("the limit label %q is drawn inside the chart", c.Text)
		}
	}
	if !containsElem(costSVGChildren(t, dom, "cost.daily"), "line", "limit") {
		t.Error("the dashed limit line is not drawn")
	}
	// The caption sits right under the legend, which is the last thing above it.
	body := costFoldChildren(t, dom, "cost.daily")
	caption := "dashed line: weekly limit $5.00/day · reset at 110%"
	legendAt, captionAt := -1, -1
	for i, c := range body {
		if c.Class == "legend" {
			legendAt = i
		}
		if c.Text == caption {
			captionAt = i
		}
	}
	if legendAt < 0 || captionAt != legendAt+1 {
		t.Errorf("the limit caption is not drawn under the legend; the fold drew %v", body)
	}
}

// The hourly chart has no limit, and it draws no text over its bars: its only
// text is the axis labels under the baseline.
func TestCostHourlyChartDrawsNoTextOverItsBars(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider()}, map[string]any{
		"spend":       spendReading(),
		"cost_charts": map[string]any{"hourly": costChartFixture(6, false, false)},
	}))
	// Six hours, every third one labelled: the labels 0 and 3 are the only text.
	var texts []string
	for _, c := range costSVGChildren(t, dom, "cost.hourly") {
		if c.Tag != "TEXT" {
			continue
		}
		if c.Class != "day" {
			t.Errorf("the hourly chart draws %q with class %q over its bars; only the axis labels belong there", c.Text, c.Class)
		}
		texts = append(texts, c.Text)
	}
	if got := strings.Join(texts, ","); got != "0,3" {
		t.Errorf("the hourly chart's text is %q, want only its axis labels 0,3", got)
	}
}

// The closed Cost header is one line built from the providers array: each
// provider's figure and status in the array's order, so a reader sees the
// balances and the limit at a glance.
func TestCostHeaderSummaryIsBuiltFromTheProvidersArray(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	s := costPanelState([]map[string]any{
		{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "balance": 27.45, "status": "warn", "status_text": "3.0d runway"},
		{"id": "claude", "label": "Claude", "kind": "plan", "plan_pct": 6.0, "status": "ok", "status_text": "limit in 5.7d"},
	}, nil)
	js, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	v, err := dom.vm.RunString("FOLD_SUMMARY.cost(" + string(js) + ")")
	if err != nil {
		t.Fatalf("the header summary: %v", err)
	}
	if want := "DeepSeek $27.45 3.0d runway · Claude 6% (wk) limit in 5.7d"; v.String() != want {
		t.Errorf("the header summary is %q, want %q", v.String(), want)
	}
}

// With no providers, or no list, the closed Cost header has no summary to show: it
// is null, not an empty line that reads as a figure.
func TestCostHeaderSummaryIsEmptyWithoutProviders(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	for name, s := range map[string]string{"absent": `{}`, "empty": `{"cost_providers": []}`, "null": `{"cost_providers": null}`} {
		v, err := dom.vm.RunString("JSON.stringify(FOLD_SUMMARY.cost(" + s + "))")
		if err != nil {
			t.Fatalf("%s: the header summary: %v", name, err)
		}
		if got := v.String(); got != "null" {
			t.Errorf("%s: the header summary is %s, want null", name, got)
		}
	}
}

// The closed Cost section shows its summary on its heading, drawn from the same
// reading the table is, and the summary is empty when the array is not sent.
func TestCostHeaderShowsItsSummaryWhenClosed(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderCost", costPanelState([]map[string]any{deepSeekProvider()}, map[string]any{"spend": spendReading()}))
	if _, err := dom.vm.RunString(`state = ` + mustJSONString(t, costPanelState([]map[string]any{deepSeekProvider()}, nil)) + `; (() => { const f = folds.filter(f => f.key === "cost")[0]; f.closed = true; paintFolds(state); })()`); err != nil {
		t.Fatalf("closing the Cost section: %v", err)
	}
	v, err := dom.vm.RunString(`JSON.stringify(__foldRead("cost"))`)
	if err != nil {
		t.Fatalf("reading the Cost heading: %v", err)
	}
	if !strings.Contains(v.String(), "DeepSeek $27.45 3.0d runway") {
		t.Errorf("the closed Cost heading does not show the providers' summary: %s", v.String())
	}
}

// The page no longer draws the markup the table replaces: the DeepSeek tiles and
// prose, the Claude headline and plan line, the Jev prose line, and the limit
// label over the chart.
func TestCostPageDropsTheMarkupTheTableReplaces(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, gone := range []string{
		"function costSpend(", "function costClaudeRow(", "function costClaudeHeadline(",
		"function costClaudePlan(", "function costJev(", "costClaudeHeadline", "costClaudePlan",
		"costClaudeRow", "costheadline", "limitlbl", "costDailyChart", "costHourlyChart",
	} {
		if strings.Contains(page, gone) {
			t.Errorf("index.html still carries %q, which the providers table replaces", gone)
		}
	}
	cost := pageFunc(t, "renderCost")
	if i, j := strings.Index(cost, "costProviders("), strings.Index(cost, "costChartFold(spendBox, \"daily\""); i < 0 || j < 0 || i > j {
		t.Error("renderCost does not draw the providers table before the daily chart")
	}
	// The budget line sits directly under the provider rows, above the spend part
	// that holds the charts.
	if i, j := strings.Index(cost, "costGCP(box, s.cloud)"), strings.Index(cost, "box.append(spendBox)"); i < 0 || j < 0 || i > j {
		t.Error("renderCost does not draw the GCP budget line under the provider rows, above the daily chart")
	}
}

// cellTexts is the text of each cell of a row, in column order.
func cellTexts(row costTableRow) []string {
	out := make([]string, 0, len(row.Cells))
	for _, c := range row.Cells {
		out = append(out, c.Text)
	}
	return out
}

// containsText reports whether want is one of the texts read back.
func containsText(texts []string, want string) bool {
	for _, t := range texts {
		if t == want || strings.Contains(t, want) {
			return true
		}
	}
	return false
}

// containsElem reports whether an element with the tag and class is among the
// elements read back.
func containsElem(elems []costElem, tag, class string) bool {
	for _, e := range elems {
		if strings.EqualFold(e.Tag, tag) && e.Class == class {
			return true
		}
	}
	return false
}

// costSVGChildren reads the children of the chart svg in the fold keyed key, with
// each one's tag and its class, which the page sets as an attribute.
func costSVGChildren(t *testing.T, dom *pageDOM, key string) []costElem {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const sec = __byId["cost"].children.filter(c => c.className === "spendpart")[0].children.filter(c => c.dataset && c.dataset.fold === ` + quoteJS(key) + `)[0];
		const svg = sec.children[1].children.filter(c => c.tagName === "SVG")[0];
		if (!svg) return [];
		return svg.children.map(c => ({tag: c.tagName, class: c.getAttribute("class") || "", text: c.textContent}));
	})())`)
	if err != nil {
		t.Fatalf("reading the %s chart: %v", key, err)
	}
	var out []costElem
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the %s chart: %v", key, err)
	}
	return out
}

// costFoldKeys lists the keys of the chart folds the Cost panel drew.
func costFoldKeys(t *testing.T, dom *pageDOM) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(__byId["cost"].children.filter(c => c.className === "spendpart")[0].children.filter(c => c.dataset && c.dataset.fold).map(c => c.dataset.fold))`)
	if err != nil {
		t.Fatalf("reading the Cost folds: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the Cost folds: %v", err)
	}
	return out
}

// foldClosed reads whether the chart fold keyed key is closed, from the folds the
// page keeps.
func foldClosed(t *testing.T, dom *pageDOM, key string) bool {
	t.Helper()

	v, err := dom.vm.RunString(`(() => { const f = folds.filter(f => f.key === ` + quoteJS(key) + `)[0]; return f ? f.closed : null; })()`)
	if err != nil {
		t.Fatalf("reading the %s fold: %v", key, err)
	}
	if v.Export() == nil {
		t.Fatalf("the page keeps no %s fold", key)
	}
	return v.ToBoolean()
}

// mustJSONString encodes a value as the JavaScript literal the page reads.
func mustJSONString(t *testing.T, v any) string {
	t.Helper()

	js, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %v: %v", v, err)
	}
	return string(js)
}
