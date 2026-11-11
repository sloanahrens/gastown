package dashboard

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const jevReport = `{"calls":14,"input_tokens":900,"output_tokens":100,"usd":1.75,"lines_skipped":0,
 "by_caller":{"deacon":{"calls":4,"usd":0.25},"overseer":{"calls":10,"usd":1.5}}}`

// The Jev row's marks are the hub's, set on the copy the rows are built from: a
// report can neither set them nor find them on the state's JSON.
func TestJevSpendMarksAreNeverSentOrRead(t *testing.T) {
	t.Parallel()

	out, err := json.Marshal(JevSpend{Calls: 1, USD: 2, ReadFailed: true, Stale: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"read_failed", "stale"} {
		if strings.Contains(string(out), key) {
			t.Errorf("the marks are sent as %q: %s", key, out)
		}
	}
	var got JevSpend
	if err := json.Unmarshal([]byte(`{"calls":1,"usd":2,"read_failed":true,"stale":true}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.ReadFailed || got.Stale {
		t.Errorf("a report sets the marks: %+v", got)
	}
}

func TestParseJevSpendKeepsTheCallsDollarsAndTopCaller(t *testing.T) {
	t.Parallel()

	got, err := ParseJevSpend([]byte(jevReport))
	if err != nil {
		t.Fatalf("ParseJevSpend: %v", err)
	}
	want := JevSpend{Calls: 14, USD: 1.75, TopCaller: "overseer", TopCallerCalls: 10, TopCallerUSD: 1.5}
	if *got != want {
		t.Errorf("row = %+v, want %+v", *got, want)
	}
}

func TestParseJevSpendTopCallerTies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, report, want string
	}{
		{"most dollars", `{"calls":3,"usd":3,"by_caller":{"a":{"calls":1,"usd":1},"b":{"calls":2,"usd":2}}}`, "b"},
		{"dollars tie, most calls", `{"calls":3,"usd":2,"by_caller":{"a":{"calls":1,"usd":1},"b":{"calls":2,"usd":1}}}`, "b"},
		{"full tie, first by name", `{"calls":2,"usd":2,"by_caller":{"b":{"calls":1,"usd":1},"a":{"calls":1,"usd":1}}}`, "a"},
		{"no callers", `{"calls":0,"usd":0,"by_caller":{}}`, ""},
		{"no by_caller", `{"calls":0,"usd":0}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseJevSpend([]byte(tc.report))
			if err != nil {
				t.Fatal(err)
			}
			if got.TopCaller != tc.want {
				t.Errorf("top caller = %q, want %q", got.TopCaller, tc.want)
			}
		})
	}
}

// Output that is not the report is a failed read: a window of no spend is a
// report with zeros, not an empty line or a bare null.
func TestParseJevSpendRefusesWhatIsNotTheReport(t *testing.T) {
	t.Parallel()

	for _, out := range []string{"", "  \n", "null", "[]", "12", "gt: unknown command", `{"calls":1}`, `{"usd":1}`, `{"calls":"x","usd":1}`, `{"calls":1,"usd":1,`} {
		if got, err := ParseJevSpend([]byte(out)); err == nil {
			t.Errorf("ParseJevSpend(%q) = %+v, want an error", out, got)
		}
	}
}

func TestJevSpendWindowReadsTheCommandsHours(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"gt jev spend --json --hours 24 --usd-per-billion-tokens 42": "24h",
		"gt jev spend --hours=168 --json":                            "168h",
		"gt jev spend --json":                                        "",
		"gt jev spend --hours":                                       "",
		"gt jev spend --hours soon":                                  "",
		"gt jev spend --hours 0":                                     "",
		"gt jev spend --hours=-3":                                    "",
	}
	for cmd, want := range cases {
		if got := JevSpendWindow(strings.Fields(cmd)); got != want {
			t.Errorf("JevSpendWindow(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// The Jev read and the DeepSeek read fail apart: a Jev read that fails marks
// only its own source, and the DeepSeek value and status are left as they were.
func TestFailedJevSpendReadKeepsTheRowAndLeavesDeepSeekAlone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	jevOK := true
	h := NewHub(Config{
		Now:   func() time.Time { return now },
		Spend: func() json.RawMessage { return json.RawMessage(`{"balance":12}`) },
		JevSpend: func() *JevSpend {
			if !jevOK {
				return nil
			}
			return &JevSpend{Calls: 3, USD: 0.5}
		},
	})

	h.pollSpend()
	h.pollJevSpend()
	if r := h.State().Reads[SourceJevSpend]; r.Error || r.Stale {
		t.Fatalf("a good Jev read is flagged: %+v", r)
	}
	jevOK = false
	h.pollJevSpend()

	st := h.State()
	if st.JevSpend == nil || st.JevSpend.Calls != 3 {
		t.Errorf("a failed read dropped the last good Jev row: %+v", st.JevSpend)
	}
	if r := st.Reads[SourceJevSpend]; !r.Error || !r.Stale {
		t.Errorf("a failed Jev read is not flagged error and stale: %+v", r)
	}
	if len(st.Spend) == 0 {
		t.Error("a failed Jev read blanked the DeepSeek spend")
	}
	if r := st.Reads[SourceSpend]; r.Error || r.Stale {
		t.Errorf("a failed Jev read marked the DeepSeek read: %+v", r)
	}

	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"jev_spend":{"calls":3`, `"jev_spend":{"at"`} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("the state payload has no %s", want)
		}
	}
}

// A Jev read that never succeeded has no row to keep: the state carries only
// the failed status, which is what the page draws "unavailable" from.
func TestJevSpendNeverReadCarriesOnlyTheFailure(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{JevSpend: func() *JevSpend { return nil }})
	h.pollJevSpend()
	st := h.State()
	if st.JevSpend != nil {
		t.Errorf("a read that failed from the start has a row: %+v", st.JevSpend)
	}
	if r := st.Reads[SourceJevSpend]; !r.Error {
		t.Errorf("the failed read is not flagged: %+v", r)
	}
}

func jevState(over map[string]any) map[string]any {
	return jevStateAt(time.Now(), over)
}

// jevStateAt is the Jev state with the Claude plan read at now. A comparison of
// two draws passes one now to both: the plan's reset text is read off the clock,
// so two builds at different instants can differ on a row the test leaves alone.
func jevStateAt(now time.Time, over map[string]any) map[string]any {
	s := map[string]any{
		"spend":       map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}},
		"jev_spend":   map[string]any{"calls": 14, "usd": 1.75, "window": "24h", "top_caller": "overseer", "top_caller_calls": 10, "top_caller_usd": 1.5},
		"claude_plan": claudePlan(now, nil),
	}
	for k, v := range over {
		if v == nil {
			delete(s, k)
		} else {
			s[k] = v
		}
	}
	return s
}

func jevRead(extra map[string]any) map[string]any {
	r := map[string]any{"at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "every_sec": 300}
	for k, v := range extra {
		r[k] = v
	}
	return map[string]any{"jev_spend": r}
}

// The scorecard read and the spend read fail apart, and a failed scorecard
// read keeps the last good lines.
func TestFailedJevScorecardReadKeepsTheLinesAndLeavesTheSpendAlone(t *testing.T) {
	t.Parallel()

	ok := true
	h := NewHub(Config{
		JevSpend: func() *JevSpend { return &JevSpend{Calls: 3, USD: 0.5} },
		JevScorecard: func() *JevScorecard {
			if !ok {
				return nil
			}
			return &JevScorecard{Batteries: []JevBattery{{Name: "finding", Items: 2, Labeled: 1}}}
		},
	})
	h.pollJevSpend()
	h.pollJevScorecard()
	if r := h.State().Reads[SourceJevScorecard]; r.Error || r.Stale {
		t.Fatalf("a good scorecard read is flagged: %+v", r)
	}
	ok = false
	h.pollJevScorecard()

	st := h.State()
	if st.JevScorecard == nil || len(st.JevScorecard.Batteries) != 1 {
		t.Errorf("a failed read dropped the last good lines: %+v", st.JevScorecard)
	}
	if r := st.Reads[SourceJevScorecard]; !r.Error || !r.Stale {
		t.Errorf("a failed scorecard read is not flagged error and stale: %+v", r)
	}
	if r := st.Reads[SourceJevSpend]; r.Error || r.Stale {
		t.Errorf("a failed scorecard read marked the spend read: %+v", r)
	}
	if st.JevSpend == nil || st.JevSpend.Calls != 3 {
		t.Errorf("a failed scorecard read changed the spend row: %+v", st.JevSpend)
	}
}

// A read that lost one battery keeps that battery's last good line and leaves
// the others as the read drew them; a battery never read stays a placeholder.
// A later good read clears the marks.
func TestHubKeepsEachBatterysLastGoodLineAcrossItsOwnFailedRead(t *testing.T) {
	t.Parallel()

	reads := [][]JevBattery{
		{{Name: "finding", Items: 2, Labeled: 1}, {Name: "landing", Items: 5}},
		{{Name: "finding", Items: 3, Labeled: 2}, {Name: "landing", Failed: true}},
		{{Name: "finding", Failed: true}, {Name: "landing", Items: 7}},
		{{Name: "finding", Items: 4}, {Name: "landing", Items: 8}},
	}
	next := 0
	h := NewHub(Config{JevScorecard: func() *JevScorecard {
		sc := &JevScorecard{Batteries: append([]JevBattery(nil), reads[next]...)}
		next++
		sc.Tally()
		return sc
	}})
	poll := func() []JevBattery {
		h.pollJevScorecard()
		return h.State().JevScorecard.Batteries
	}

	poll()
	got := poll()
	if want := (JevBattery{Name: "landing", Items: 5, Failed: true, Kept: true}); !reflect.DeepEqual(got[1], want) {
		t.Errorf("landing after its failed read = %+v, want its last good line %+v", got[1], want)
	}
	if got[0].Items != 3 || got[0].Failed {
		t.Errorf("finding was touched by landing's failed read: %+v", got[0])
	}
	if sc := h.State().JevScorecard; !reflect.DeepEqual(sc.Unavailable, []string{"landing"}) || sc.Totals.Items != 8 || sc.Totals.Batteries != 2 {
		t.Errorf("scorecard = %+v, want landing named unavailable and its kept line counted", sc)
	}

	got = poll()
	if got[0].Items != 3 || got[0].Labeled != 2 || !got[0].Kept || got[1].Items != 7 || got[1].Failed {
		t.Errorf("after finding's failed read: %+v", got)
	}

	got = poll()
	if got[0].Items != 4 || got[0].Failed || got[1].Items != 8 || got[1].Failed {
		t.Errorf("a good read did not clear the marks: %+v", got)
	}
	if r := h.State().Reads[SourceJevScorecard]; r.Error || r.Stale {
		t.Errorf("a partial read flagged the whole source: %+v", r)
	}
}

// A kept line is dropped to the battery's name once its last good read is older
// than jevKeptMaxAge; a good read in between restarts the clock.
func TestHubDropsAKeptLineOlderThanItsAgeBound(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	reads := []bool{true, false, false, false}
	next := 0
	h := NewHub(Config{
		Now: func() time.Time { return now },
		JevScorecard: func() *JevScorecard {
			ok := reads[next]
			next++
			sc := &JevScorecard{Batteries: []JevBattery{{Name: "landing", Items: 5}}}
			if !ok {
				sc.Batteries[0] = JevBattery{Name: "landing", Failed: true}
			}
			sc.Tally()
			return sc
		},
	})
	poll := func() JevBattery {
		h.pollJevScorecard()
		return h.State().JevScorecard.Batteries[0]
	}

	poll()
	now = now.Add(jevKeptMaxAge)
	if got := poll(); !got.Kept || got.Items != 5 {
		t.Errorf("a line exactly at its bound = %+v, want it kept", got)
	}
	now = now.Add(time.Second)
	if got := poll(); got.Kept || got.Items != 0 || !got.Failed {
		t.Errorf("a line past its bound = %+v, want the name alone", got)
	}
	if got := poll(); got.Kept {
		t.Errorf("an expired line came back on a later failed read: %+v", got)
	}
}

// A battery the hub never read a good line for stays a bare placeholder.
func TestHubLeavesAPlaceholderForABatteryItNeverRead(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{JevScorecard: func() *JevScorecard {
		sc := &JevScorecard{Batteries: []JevBattery{{Name: "finding", Items: 1}, {Name: "landing", Failed: true}}}
		sc.Tally()
		return sc
	}})
	h.pollJevScorecard()
	h.pollJevScorecard()
	got := h.State().JevScorecard.Batteries
	if want := (JevBattery{Name: "landing", Failed: true}); !reflect.DeepEqual(got[1], want) {
		t.Errorf("landing = %+v, want %+v", got[1], want)
	}
}

// jevDrawn is one element a JEV row's questions cell holds: a question's chip,
// the fold's chip button, or the folded list of not-yet chips.
type jevDrawn struct {
	Tag    string     `json:"tag"`
	Class  string     `json:"class"`
	Text   string     `json:"text"`
	Title  string     `json:"title"`
	Hidden bool       `json:"hidden"`
	Kids   []jevDrawn `json:"kids"`
}

// jevCellDrawn is one cell of a JEV row, with what its questions cell holds.
type jevCellDrawn struct {
	Class string     `json:"class"`
	Text  string     `json:"text"`
	Kids  []jevDrawn `json:"kids"`
}

// jevRowDrawn is one row of the JEV table.
type jevRowDrawn struct {
	Class string         `json:"class"`
	Cells []jevCellDrawn `json:"cells"`
}

// The JEV panel draws the batteries a partial read kept as table rows, names the
// lost ones on the title line, and marks a lost line stale or unavailable.
func TestJevPanelNamesTheBatteriesTheReadLost(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevScorecardState(now, nil)
	sc := s["jev_scorecard"].(map[string]any)
	scored := now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	sc["batteries"] = []any{
		map[string]any{"name": "finding", "items": 212, "labeled": 140},
		map[string]any{"name": "dispatch", "items": 4, "labeled": 1, "scored_at": scored, "failed": true, "kept": true},
		map[string]any{"name": "drift", "failed": true},
	}
	sc["unavailable"] = []any{"dispatch", "drift"}
	sc["totals"] = map[string]any{"batteries": 2, "items": 216, "labeled": 141}

	rows, note, _ := drawJevAt(t, now, s)
	got := jevRowTexts(rows)
	want := []string{
		"dispatchscored 5h00m ago41 (kept: the last refresh failed)",
		"drift--unavailable",
		"finding212140",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JEV rows = %q, want %q", got, want)
	}
	if wantNote := "2 batteries · 216 items · 141 labeled · last 7d · 2 unavailable: dispatch, drift"; note.Text != wantNote {
		t.Errorf("JEV title line = %q, want %q", note.Text, wantNote)
	}
	if note.Class != "r warnc" {
		t.Errorf("a title line naming lost batteries wears %q, want the amber class", note.Class)
	}
}

// jevScorecardState is the JEV panel's read of three batteries: finding, with
// an earning and a not-yet question; landing, with no log; and targets, with
// one no-signal question. The totals are what Tally makes of them, which
// TestJevScorecardTallyCountsTheTitleLine pins.
func jevScorecardState(now time.Time, read map[string]any) map[string]any {
	scored := now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	over := map[string]any{
		"reads": jevRead(nil),
		"jev_scorecard": map[string]any{"window": "7d", "batteries": []any{
			map[string]any{"name": "finding", "items": 212, "labeled": 140, "scored_at": scored, "questions": []any{
				map[string]any{"id": "fix_worthy", "verdict": "earning", "labeled": 140, "base_rate": 0.5, "precision": 0.75},
				map[string]any{"id": "any_duplicate", "verdict": "not-yet", "labeled": 40}}},
			map[string]any{"name": "landing", "items": 0, "labeled": 0, "scored_at": scored},
			map[string]any{"name": "targets", "items": 9, "labeled": 0, "scored_at": scored, "questions": []any{
				map[string]any{"id": "needs_review", "verdict": "no-signal", "labeled": 9, "base_rate": 0.111}}},
		}, "totals": map[string]any{"batteries": 3, "items": 221, "labeled": 140, "earning": 1, "no_signal": 1, "not_yet": 1}},
	}
	reads := over["reads"].(map[string]any)
	reads["jev_scorecard"] = map[string]any{"at": now.Add(-2 * time.Minute).UTC().Format(time.RFC3339), "every_sec": 900}
	for k, v := range read {
		reads["jev_scorecard"].(map[string]any)[k] = v
	}
	return jevStateAt(now, over)
}

// jevFindTableJS is the battery table's finder: the panel's header, groups and
// fold sit around the table, so it is found by its class, not its place.
const jevFindTableJS = `function jevFindTable(n) {
  if (n.tagName === "TABLE" && n.className === "jvtable") return n;
  for (var i = 0; i < n.children.length; i++) {
    var f = jevFindTable(n.children[i]);
    if (f) return f;
  }
  return null;
}`

// jevTableJS reads the panel's table back as rows of cells, each cell with what
// it holds. A panel that drew no table reads as no rows.
const jevTableJS = `JSON.stringify((function () {
  ` + jevFindTableJS + `
  var t = jevFindTable(__byId["jev"]), rows = [];
  if (!t) return rows;
  for (var i = 0; i < t.children.length; i++) {
    var g = t.children[i];
    if (g.tagName === "THEAD") continue;
    for (var j = 0; j < g.children.length; j++) {
      var tr = g.children[j], cells = [];
      for (var k = 0; k < tr.children.length; k++) {
        var td = tr.children[k], kids = [];
        for (var m = 0; m < td.children.length; m++) {
          var c = td.children[m], sub = [];
          for (var n = 0; n < c.children.length; n++) {
            var d = c.children[n];
            sub.push({tag: d.tagName, class: d.className, text: d.textContent, title: d.title, hidden: !!d.hidden, kids: []});
          }
          kids.push({tag: c.tagName, class: c.className, text: c.textContent, title: c.title, hidden: !!c.hidden, kids: sub});
        }
        cells.push({class: td.className, text: td.textContent, kids: kids});
      }
      rows.push({class: tr.className, cells: cells});
    }
  }
  return rows;
})())`

// drawJev runs renderJev over s and reads back the JEV panel's rows, its title
// line and the panel itself.
func drawJev(t *testing.T, s map[string]any) (rows []jevRowDrawn, note, sec drawn) {
	t.Helper()
	return drawJevIn(t, newPageDOM(t), s)
}

// drawJevAt is drawJev on a page whose clock stands at now, so a title line
// reading its age off the clock is the test's to predict.
func drawJevAt(t *testing.T, now time.Time, s map[string]any) (rows []jevRowDrawn, note, sec drawn) {
	t.Helper()
	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	return drawJevIn(t, dom, s)
}

// drawJevIn is drawJev on a page the caller keeps, so a test can click what the
// panel drew and read it again.
func drawJevIn(t *testing.T, dom *pageDOM, s map[string]any) (rows []jevRowDrawn, note, sec drawn) {
	t.Helper()
	dom.call(t, "renderJev", s)
	v, err := dom.vm.RunString(jevTableJS)
	if err != nil {
		t.Fatalf("reading the JEV panel: %v", err)
	}
	if err := json.Unmarshal([]byte(v.String()), &rows); err != nil {
		t.Fatalf("reading the JEV panel: %v", err)
	}
	return rows, dom.element(t, "jevnote"), dom.element(t, "jevsec")
}

// jevHeadCells reads the panel's column headings in the order it drew them.
func jevHeadCells(t *testing.T, dom *pageDOM) []string {
	t.Helper()
	v, err := dom.vm.RunString(`JSON.stringify((function () {
  ` + jevFindTableJS + `
  var t = jevFindTable(__byId["jev"]), out = [];
  for (var i = 0; i < t.children.length; i++) {
    if (t.children[i].tagName !== "THEAD") continue;
    var head = t.children[i].children[0];
    for (var j = 0; j < head.children.length; j++) out.push(head.children[j].textContent);
  }
  return out;
})())`)
	if err != nil {
		t.Fatalf("reading the JEV panel's headings: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the JEV panel's headings: %v", err)
	}
	return out
}

// jevRowTexts is each row's text: its cells side by side, as the table reads.
func jevRowTexts(rows []jevRowDrawn) []string {
	var out []string
	for _, r := range rows {
		var s string
		for _, c := range r.Cells {
			s += c.Text
		}
		out = append(out, s)
	}
	return out
}

// jevNoteChips reads the title line's chips in the order it drew them.
func jevNoteChips(t *testing.T, dom *pageDOM) []jevDrawn {
	t.Helper()
	v, err := dom.vm.RunString(`JSON.stringify(__byId["jevnote"].children.__arr.filter(c => c.className && c.className.indexOf("jvchip") === 0).map(c => ({tag: c.tagName, class: c.className, text: c.textContent, title: c.title})))`)
	if err != nil {
		t.Fatalf("reading the JEV title line's chips: %v", err)
	}
	var chips []jevDrawn
	if err := json.Unmarshal([]byte(v.String()), &chips); err != nil {
		t.Fatalf("reading the JEV title line's chips: %v", err)
	}
	return chips
}

// jevFoldTitle reads the tooltip on row's not-yet chip.
func jevFoldTitle(t *testing.T, dom *pageDOM, row int) string {
	t.Helper()
	v, err := dom.vm.RunString(fmt.Sprintf(`(function () {
  `+jevFindTableJS+`
  var td = jevFindTable(__byId["jev"]).children[1].children[%d].children[3];
  for (var i = 0; i < td.children.length; i++) {
    if (td.children[i].tagName === "BUTTON") return td.children[i].title;
  }
  return null;
})()`, row))
	if err != nil {
		t.Fatalf("reading a not-yet chip's tooltip: %v", err)
	}
	return v.String()
}

// jevClickFold clicks row's not-yet chip and reports whether the row's folded
// list is hidden afterwards.
func jevClickFold(t *testing.T, dom *pageDOM, row int) bool {
	t.Helper()
	v, err := dom.vm.RunString(fmt.Sprintf(`(function () {
  `+jevFindTableJS+`
  var td = jevFindTable(__byId["jev"]).children[1].children[%d].children[3], btn = null, more = null;
  for (var i = 0; i < td.children.length; i++) {
    if (td.children[i].tagName === "BUTTON") btn = td.children[i];
    if (td.children[i].className === "jvmore") more = td.children[i];
  }
  __click(btn);
  return !!more.hidden;
})()`, row))
	if err != nil {
		t.Fatalf("clicking a not-yet chip: %v", err)
	}
	return v.ToBoolean()
}

// jevTexts is the text of the elements one questions cell holds.
func jevTexts(kids []jevDrawn) []string {
	var out []string
	for _, k := range kids {
		out = append(out, k.Text)
	}
	return out
}

// The JEV panel draws one table row per battery, its name and counts then its
// questions as chips: the earning and no-signal ones shown, the not-yet ones
// folded behind a count chip. The rows are in name order.
func TestJevPanelDrawsOneRowPerBatteryInNameOrder(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevScorecardState(now, nil)
	s["jev_scorecard"].(map[string]any)["batteries"] = []any{
		map[string]any{"name": "targets", "items": 9, "questions": []any{
			map[string]any{"id": "needs_review", "verdict": "no-signal"}}},
		map[string]any{"name": "finding", "items": 212, "labeled": 140, "questions": []any{
			map[string]any{"id": "fix_worthy", "verdict": "earning"},
			map[string]any{"id": "any_duplicate", "verdict": "not-yet"}}},
		map[string]any{"name": "landing"},
	}

	rows, note, sec := drawJevAt(t, now, s)
	if sec.Hidden {
		t.Error("the JEV panel is hidden under a readable scorecard")
	}
	if len(rows) != 3 {
		t.Fatalf("the panel drew %d rows, want one per battery", len(rows))
	}
	for i, want := range []string{"finding", "landing", "targets"} {
		if got := rows[i].Cells[0].Text; got != want {
			t.Errorf("row %d is %q, want %q: the rows are not in name order", i, got, want)
		}
	}

	finding := rows[0].Cells
	if finding[1].Text != "212" || finding[2].Text != "140" {
		t.Errorf("finding's counts = %q, %q, want 212 and 140", finding[1].Text, finding[2].Text)
	}
	if got, want := finding[3].Class, "jvq"; got != want {
		t.Errorf("finding's questions cell wears %q, want %q", got, want)
	}
	if got, want := jevTexts(finding[3].Kids), []string{"fix_worthy", "+1 not yet", "any_duplicate"}; !reflect.DeepEqual(got, want) {
		t.Errorf("finding's chips = %q, want %q", got, want)
	}

	// A battery with no log in the window is a dash row and holds no chips.
	landing := rows[1].Cells
	if landing[1].Text != "-" || landing[2].Text != "-" || len(landing[3].Kids) != 0 {
		t.Errorf("a battery with no log drew %+v, want a dash row", landing)
	}

	if got, want := (jevRowTexts(rows))[2], "targets90needs_review"; got != want {
		t.Errorf("targets' row = %q, want %q", got, want)
	}
	if wantNote := "3 batteries · 221 items · 140 labeled · last 7d"; note.Text != wantNote {
		t.Errorf("JEV title line = %q, want %q", note.Text, wantNote)
	}
}

// The packages battery's row says what its items are, and the title line leaves
// it out so one battery's packages cannot swamp a headline about items.
func TestJevPanelSaysThePackagesBatteryInItsOwnRow(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevScorecardState(now, nil)
	sc := s["jev_scorecard"].(map[string]any)
	sc["batteries"] = []any{
		map[string]any{"name": "finding", "items": 212, "labeled": 140},
		map[string]any{"name": "testsel", "items": 39858, "labeled": 30000, "unit": "packages",
			"questions": []any{map[string]any{"id": "affects_tests", "verdict": "earning"}}},
	}
	sc["totals"] = map[string]any{"batteries": 1, "items": 212, "labeled": 140, "earning": 1}

	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	rows, note, _ := drawJevIn(t, dom, s)
	if got, want := jevRowTexts(rows), []string{"finding212140", "testsel39858 packages30000affects_tests"}; !reflect.DeepEqual(got, want) {
		t.Errorf("JEV rows = %q, want %q", got, want)
	}
	if got, want := jevHeadCells(t, dom), []string{"battery", "items", "labeled", "questions"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the table's headings = %q, want %q", got, want)
	}
	if wantNote := "1 battery · 212 items · 140 labeled · last 7d"; note.Text != wantNote {
		t.Errorf("JEV title line = %q, want %q", note.Text, wantNote)
	}
	if strings.Contains(note.Text, "39858") {
		t.Errorf("the packages battery is counted in the title line: %q", note.Text)
	}
}

// The title line says which window the scorecard scored, both off the data: a
// 36h scorecard says 36h, and nothing says 7d on its own. The scores' age is
// the header's, read off the summary, not the title's.
func TestJevTitleLineStatesTheWindow(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevScorecardState(now, nil)
	s["jev_scorecard"].(map[string]any)["window"] = "36h"

	_, note, _ := drawJevAt(t, now, s)
	if !strings.Contains(note.Text, "last 36h") {
		t.Errorf("the title line %q does not say last 36h", note.Text)
	}
	if strings.Contains(note.Text, "7d") {
		t.Errorf("the title line %q states a window the scorecard was not read over", note.Text)
	}
}

// A battery whose report skipped lines says how many went unanswered, and one
// that skipped none says nothing.
func TestJevPanelSaysHowManyLinesWentUnanswered(t *testing.T) {
	t.Parallel()

	s := jevScorecardState(time.Now(), nil)
	s["jev_scorecard"].(map[string]any)["batteries"] = []any{
		map[string]any{"name": "rules", "items": 25, "unanswered": 53},
		map[string]any{"name": "landing", "items": 5},
	}
	rows, _, _ := drawJev(t, s)
	cells := map[string][]jevCellDrawn{}
	for _, r := range rows {
		cells[r.Cells[0].Text] = r.Cells
	}
	if kids := cells["rules"][3].Kids; len(kids) != 1 || kids[0].Class != "empty" || kids[0].Text != "53 unanswered" || kids[0].Title != "these lines logged no answer" {
		t.Errorf("rules' questions cell holds %+v, want the unanswered count and why it is there", kids)
	}
	if kids := cells["landing"][3].Kids; len(kids) != 0 {
		t.Errorf("a battery with no skipped lines drew %+v, want nothing", kids)
	}
}

// A question's chip carries the figures the scorecard reported for it, and none
// of the ones it left out.
func TestJevQuestionChipCarriesTheReportsFigures(t *testing.T) {
	t.Parallel()

	rows, _, _ := drawJev(t, jevScorecardState(time.Now(), nil))
	finding := rows[0].Cells[3].Kids
	if got, want := finding[0].Title, "labeled 140 · base rate 50.0% · precision at 0.5 75.0%"; got != want {
		t.Errorf("the earning chip's tooltip = %q, want %q", got, want)
	}
	if got, want := finding[1].Class, "jvchip jv-notyet jvfold"; got != want {
		t.Errorf("the fold chip wears %q, want %q", got, want)
	}
	if got, want := finding[2].Kids[0].Title, "labeled 40"; got != want {
		t.Errorf("the not-yet chip's tooltip = %q, want %q: the report gives it no base rate", got, want)
	}
	if got, want := rows[2].Cells[3].Kids[0].Title, "labeled 9 · base rate 11.1%"; got != want {
		t.Errorf("the no-signal chip's tooltip = %q, want %q", got, want)
	}
}

// A row's not-yet questions stay folded until its chip is clicked, the fold
// shows how many there are, and the choice survives the draw that follows.
func TestJevPanelFoldsNotYetQuestionsUntilOpened(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	rows, _, _ := drawJevIn(t, dom, jevScorecardState(time.Now(), nil))
	if fold := rows[0].Cells[3].Kids[1]; len(fold.Kids) != 0 {
		t.Errorf("the fold chip holds the questions instead of the list: %+v", fold)
	}
	more := rows[0].Cells[3].Kids[2]
	if more.Class != "jvmore" || !more.Hidden || more.Text != "any_duplicate" {
		t.Fatalf("finding's not-yet questions are not folded: %+v", more)
	}

	const show, hide = "show this battery's not-yet questions", "hide this battery's not-yet questions"
	if got := jevFoldTitle(t, dom, 0); got != show {
		t.Errorf("the folded chip's tooltip = %q, want %q", got, show)
	}
	if hidden := jevClickFold(t, dom, 0); hidden {
		t.Error("clicking the not-yet chip did not open the fold")
	}
	if got := jevFoldTitle(t, dom, 0); got != hide {
		t.Errorf("the opened chip's tooltip = %q, want %q", got, hide)
	}
	rows, _, _ = drawJevIn(t, dom, jevScorecardState(time.Now(), nil))
	if more := rows[0].Cells[3].Kids[2]; more.Hidden {
		t.Error("the fold folded itself back on the next draw")
	}
	if got := jevFoldTitle(t, dom, 0); got != hide {
		t.Errorf("the redrawn open chip's tooltip = %q, want %q", got, hide)
	}

	if hidden := jevClickFold(t, dom, 0); !hidden {
		t.Error("clicking the not-yet chip again did not fold the list")
	}
	if got := jevFoldTitle(t, dom, 0); got != show {
		t.Errorf("the refolded chip's tooltip = %q, want %q", got, show)
	}
}

// A verdict the page does not know is shown beside the earning and no-signal
// questions in the dim colour, not folded: the title counts only not-yet under
// that name, so the fold's count must match it.
func TestJevPanelShowsAnUnknownVerdictInsteadOfFoldingIt(t *testing.T) {
	t.Parallel()

	s := jevScorecardState(time.Now(), nil)
	s["jev_scorecard"].(map[string]any)["batteries"] = []any{
		map[string]any{"name": "drift", "items": 40, "questions": []any{
			map[string]any{"id": "new_one", "verdict": "borderline"},
			map[string]any{"id": "sections_stable", "verdict": "not-yet"}}},
	}
	rows, _, _ := drawJev(t, s)
	kids := rows[0].Cells[3].Kids
	if got, want := jevTexts(kids), []string{"new_one", "+1 not yet", "sections_stable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a battery with an unknown verdict drew %q, want %q", got, want)
	}
	if got, want := kids[0].Class, "jvchip jv-notyet"; got != want {
		t.Errorf("the unknown verdict's chip wears %q, want %q", got, want)
	}
}

// A battery whose every question is not-yet shows the count alone, without the
// plus that says other questions are shown beside it.
func TestJevPanelShowsTheCountAloneWhenEveryQuestionIsNotYet(t *testing.T) {
	t.Parallel()

	s := jevScorecardState(time.Now(), nil)
	s["jev_scorecard"].(map[string]any)["batteries"] = []any{
		map[string]any{"name": "drift", "items": 40, "questions": []any{
			map[string]any{"id": "sections_stable", "verdict": "not-yet"},
			map[string]any{"id": "doc_follows", "verdict": "not-yet"}}},
	}
	rows, _, _ := drawJev(t, s)
	kids := rows[0].Cells[3].Kids
	if len(kids) != 2 || kids[0].Text != "2 not yet" || kids[1].Class != "jvmore" || len(kids[1].Kids) != 2 {
		t.Errorf("a battery with only not-yet questions drew %+v, want \"2 not yet\" over its two chips", kids)
	}
}

// jevFindClassJS finds the first element under n whose class is cls. The groups
// sit in divs around the battery table, so the page's groups are found by class.
const jevFindClassJS = `function jevFindClass(n, cls) {
  if (n.className === cls) return n;
  for (var i = 0; i < n.children.length; i++) {
    var f = jevFindClass(n.children[i], cls);
    if (f) return f;
  }
  return null;
}`

// jevFindAllJS collects every element under n whose class is cls, in the order
// the panel drew them.
const jevFindAllJS = `function jevFindAll(n, cls, out) {
  if (n.className === cls) out.push(n);
  for (var i = 0; i < n.children.length; i++) jevFindAll(n.children[i], cls, out);
  return out;
}`

// jevSummaryState is the three-battery panel with the server's summary over it:
// finding's earning question and its watching question, three stuck questions
// under their causes, targets' no-signal question for retirement, and a battery
// the summary lists as off. Today's spend is 55 cents of a dollar cap, and the
// scores were computed five hours ago.
func jevSummaryState(now time.Time) map[string]any {
	s := jevScorecardState(now, nil)
	sc := s["jev_scorecard"].(map[string]any)
	sc["batteries"] = append(sc["batteries"].([]any), map[string]any{"name": "retired", "items": 3, "labeled": 3})
	s["jev_summary"] = map[string]any{
		"refreshed": now.Add(-5 * time.Hour).UTC().Format(time.RFC3339),
		"off":       []any{"retired"},
		"earning": []any{
			map[string]any{"battery": "finding", "id": "fix_worthy", "labeled": 140, "positives": 70, "base_rate": 0.5, "precision_at_cut": 0.75, "lift": 0.25, "used_by": "gt attention and dashboard queue order"},
		},
		"watching": []any{
			map[string]any{"name": "finding.any_duplicate", "labeled": 40, "positives": 4, "negatives": 36},
		},
		"stuck": map[string]any{
			"no_label_rule":  map[string]any{"count": 1, "names": []any{"landing.risk"}},
			"only_positive":  map[string]any{"count": 0},
			"zero_positives": map[string]any{"count": 2, "names": []any{"rework.all_no", "rework.dup"}},
		},
		"retire": []any{map[string]any{"battery": "targets", "questions": []any{"needs_review"}}},
		"spend":  map[string]any{"window": "24h", "calls": 14, "window_usd": 1.75, "today_usd": 0.55, "cap_usd": 1.0},
	}
	return s
}

// jevClassText is the text of the first element of the JEV panel with class cls,
// or "<none>" when the panel draws no such element.
func jevClassText(t *testing.T, dom *pageDOM, cls string) string {
	t.Helper()
	v, err := dom.vm.RunString(`(function () {
  ` + jevFindClassJS + `
  var n = jevFindClass(__byId["jev"], "` + cls + `");
  return n ? n.textContent : "<none>";
})()`)
	if err != nil {
		t.Fatalf("reading the JEV panel's %s: %v", cls, err)
	}
	return v.String()
}

// jevEarnCells reads the EARNING table back: each row's cells with their text,
// class and tooltip. A panel that drew no earning table reads as no rows.
func jevEarnCells(t *testing.T, dom *pageDOM) [][]jevDrawn {
	t.Helper()
	v, err := dom.vm.RunString(`JSON.stringify((function () {
  ` + jevFindClassJS + `
  var tbl = jevFindClass(__byId["jev"], "jvearn"), out = [];
  if (!tbl) return out;
  var body = tbl.children[1];
  for (var i = 0; i < body.children.length; i++) {
    var tr = body.children[i], cells = [];
    for (var j = 0; j < tr.children.length; j++) {
      var td = tr.children[j];
      cells.push({tag: td.tagName, class: td.className, text: td.textContent, title: td.title, hidden: false, kids: []});
    }
    out.push(cells);
  }
  return out;
})())`)
	if err != nil {
		t.Fatalf("reading the JEV earning table: %v", err)
	}
	var out [][]jevDrawn
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the JEV earning table: %v", err)
	}
	return out
}

// jevWrapDrawn is one fold the panel draws: the chip that opens it, whether its
// list is hidden, and the text of each item the list holds.
type jevWrapDrawn struct {
	Button string   `json:"button"`
	Hidden bool     `json:"hidden"`
	Items  []string `json:"items"`
}

// jevWrap reads the idx-th fold of class cls, clicking its chip first when click
// is set. A fold is a wrapper: its chip is the first child, its list the second.
func jevWrap(t *testing.T, dom *pageDOM, cls string, idx int, click bool) jevWrapDrawn {
	t.Helper()
	v, err := dom.vm.RunString(fmt.Sprintf(`JSON.stringify((function () {
  `+jevFindAllJS+`
  var w = jevFindAll(__byId["jev"], %s, [])[%d];
  if (%t) __click(w.children[0]);
  var list = w.children[1], items = [];
  for (var i = 0; i < list.children.length; i++) items.push(list.children[i].textContent);
  return {button: w.children[0].textContent, hidden: !!list.hidden, items: items};
})())`, strconv.Quote(cls), idx, click))
	if err != nil {
		t.Fatalf("reading the JEV panel's %s fold %d: %v", cls, idx, err)
	}
	var got jevWrapDrawn
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the JEV panel's %s fold %d: %v", cls, idx, err)
	}
	return got
}

// The header line says when the scores were computed, how many batteries are ok
// and how many are off. The spend bar draws today's dollars against the cap
// with the top caller beside it. EARNING draws each earning question with its
// lift, labeled count and consumer, and a battery the summary lists as off takes
// no row.
func TestJevPageShowsHeaderSpendAndEarningTable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	rows, _, _ := drawJevIn(t, dom, jevSummaryState(now))

	if got, want := jevClassText(t, dom, "jvtitle"), "scored 5h00m ago · 3 batteries ok · 1 off"; got != want {
		t.Errorf("the header line = %q, want %q", got, want)
	}
	if got, want := jevClassText(t, dom, "jvspend"), "$0.55 of $1.00 · top caller overseer (10 calls)"; got != want {
		t.Errorf("the spend line = %q, want %q", got, want)
	}
	v, err := dom.vm.RunString(`(function () { ` + jevFindClassJS + ` return jevFindClass(__byId["jev"], "jvbar").children[0].style.width; })()`)
	if err != nil {
		t.Fatalf("reading the spend bar: %v", err)
	}
	if got, want := v.String(), "55.0%"; got != want {
		t.Errorf("the spend bar fills %q, want %q", got, want)
	}

	cells := jevEarnCells(t, dom)
	if len(cells) != 1 {
		t.Fatalf("EARNING drew %d rows, want the one earning question", len(cells))
	}
	var got []string
	for _, c := range cells[0] {
		got = append(got, c.Text)
	}
	if want := []string{"finding.fix_worthy", "+25.0%", "140", "gt attention and dashboard queue order"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EARNING row = %q, want %q", got, want)
	}

	if len(rows) != 3 {
		t.Fatalf("the battery table drew %d rows, want the three batteries that are not off", len(rows))
	}
	for _, r := range rows {
		if r.Cells[0].Text == "retired" {
			t.Errorf("the battery the summary lists as off drew a row: %q", jevRowTexts(rows))
		}
	}
}

// The battery table is folded under "all batteries" and closed until it is
// opened. The STUCK causes list their questions only when their own count is
// opened, and a choice survives the redraw the panel gets on every event. The
// WATCHING group is not folded.
func TestJevPageFoldsTheBatteryTableAndNotYetGroups(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	drawJevIn(t, dom, jevSummaryState(now))

	if all := jevWrap(t, dom, "jvall", 0, false); !all.Hidden || all.Button != "all batteries" {
		t.Errorf("the battery table's fold = %+v, want closed under all batteries", all)
	}
	for i, want := range []string{"1 no label rule", "0 only positive labels", "2 zero positives"} {
		if got := jevWrap(t, dom, "jvcause", i, false); got.Button != want || !got.Hidden {
			t.Errorf("STUCK cause %d = %+v, want %q with its list folded", i, got, want)
		}
	}

	if got := jevWrap(t, dom, "jvcause", 2, true); got.Hidden || !reflect.DeepEqual(got.Items, []string{"rework.all_no", "rework.dup"}) {
		t.Errorf("zero positives opened to %+v, want its two questions", got)
	}
	if got := jevWrap(t, dom, "jvcause", 0, false); !got.Hidden {
		t.Error("opening zero positives also opened no label rule")
	}

	drawJevIn(t, dom, jevSummaryState(now))
	if got := jevWrap(t, dom, "jvcause", 2, false); got.Hidden {
		t.Error("the zero positives list folded itself back on the next draw")
	}
	if all := jevWrap(t, dom, "jvall", 0, true); all.Hidden {
		t.Error("clicking all batteries did not open the battery table")
	}
	if all := jevWrap(t, dom, "jvall", 0, false); all.Hidden {
		t.Error("the battery table folded itself back on the next draw")
	}

	v, err := dom.vm.RunString(`__byId["jev"].textContent`)
	if err != nil {
		t.Fatalf("reading the JEV panel: %v", err)
	}
	if !strings.Contains(v.String(), "finding.any_duplicate · labeled 40 · 4 positive, 36 negative") {
		t.Errorf("WATCHING does not draw its question in the open: %q", v.String())
	}
}

// The title line keeps the totals and the window, and drops the oldest score's
// age and the verdict counts: the header carries the age, the groups the verdicts.
func TestJevPageDropsTheOldestScoreTitleAndVerdictChips(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	_, note, _ := drawJevIn(t, dom, jevSummaryState(now))

	if want := "3 batteries · 221 items · 140 labeled · last 7d"; note.Text != want {
		t.Errorf("JEV title line = %q, want %q", note.Text, want)
	}
	if chips := jevNoteChips(t, dom); len(chips) != 0 {
		t.Errorf("the title line draws %d verdict chips, want none: %+v", len(chips), chips)
	}
	for _, word := range []string{"oldest", "score age"} {
		if strings.Contains(note.Text, word) || strings.Contains(jevClassText(t, dom, "jvtitle"), word) {
			t.Errorf("the JEV panel says %q: title %q", word, note.Text)
		}
	}
}

// An earning question with no consumer is the row that asks for one: its used-by
// cell is drawn in the warning colour, with the reason as its tooltip. A consumer
// is drawn plain, and a score question has no lift and says why.
func TestJevPageMarksAnEarningRowWithNoConsumer(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevSummaryState(now)
	s["jev_summary"].(map[string]any)["earning"] = []any{
		map[string]any{"battery": "finding", "id": "fix_worthy", "labeled": 140, "positives": 70, "base_rate": 0.5, "precision_at_cut": 0.75, "lift": 0.25, "used_by": "none"},
		map[string]any{"battery": "landing", "id": "risk", "labeled": 80, "positives": 30, "base_rate": 0.4, "precision_at_cut": 0.55, "lift": 0.15, "used_by": "risk-path order"},
		map[string]any{"battery": "progress", "id": "looping", "labeled": 50, "positives": 25, "no_lift": "a score question has no base rate to lift over", "used_by": "none"},
	}
	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	drawJevIn(t, dom, s)

	cells := jevEarnCells(t, dom)
	if len(cells) != 3 {
		t.Fatalf("EARNING drew %d rows, want 3", len(cells))
	}
	if used := cells[0][3]; used.Class != "warnc" || used.Text != "none" || used.Title != "no gate or queue reads this question: it needs a consumer" {
		t.Errorf("the row with no consumer drew its used-by cell as %+v, want the warning colour and the reason", used)
	}
	if used := cells[1][3]; used.Class != "" || used.Text != "risk-path order" {
		t.Errorf("the row with a consumer drew its used-by cell as %+v, want it plain", used)
	}
	if lift := cells[2][1]; lift.Class != "empty" || lift.Text != "a score question has no base rate to lift over" {
		t.Errorf("a score question's lift cell = %+v, want the reason it has no lift", lift)
	}
	if used := cells[2][3]; used.Class != "warnc" {
		t.Errorf("a score question with no consumer is not in the warning colour: %+v", used)
	}
}

// A battery the refresh reused says when its log was last unchanged beside its
// name, and is never drawn as stale or old.
func TestJevPageDrawsAReusedBatteryAsUnchangedNotStale(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevSummaryState(now)
	reusedAt := now.Add(-48 * time.Hour).UTC()
	s["jev_summary"].(map[string]any)["reused"] = []any{
		map[string]any{"battery": "finding", "scored_at": reusedAt.Format(time.RFC3339), "reason": "log unchanged"},
	}
	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	rows, _, _ := drawJevIn(t, dom, s)

	finding := rows[0]
	if got, want := finding.Cells[0].Kids[1].Text, "unchanged since "+reusedAt.Format("2006-01-02"); got != want {
		t.Errorf("the reused battery's name line = %q, want %q", got, want)
	}
	if strings.Contains(finding.Cells[0].Text, "scored") {
		t.Errorf("the reused battery says it was scored an age ago: %q", finding.Cells[0].Text)
	}
	if finding.Class == "jvstale" {
		t.Error("the reused battery is drawn as stale")
	}
}

// Without the summary the groups are left out and the page says so, while the
// battery table still draws from the scorecard, and the header and spend line
// fall back to what the reads carry.
func TestJevPageWithoutTheSummaryLeavesTheGroupsOut(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dom := newPageDOM(t)
	pageExpr(t, dom, "__epoch = "+strconv.FormatInt(now.UnixMilli(), 10))
	rows, _, _ := drawJevIn(t, dom, jevScorecardState(now, nil))

	if got := jevClassText(t, dom, "jvearn"); got != "<none>" {
		t.Errorf("the earning table is drawn without a summary: %q", got)
	}
	if got := jevClassText(t, dom, "jvall"); got == "<none>" {
		t.Error("the battery table is left out without a summary")
	}
	if len(rows) != 3 {
		t.Errorf("the battery table drew %d rows, want the three batteries", len(rows))
	}
	if got, want := jevClassText(t, dom, "jvtitle"), "score time unknown · 3 batteries ok · 0 off"; got != want {
		t.Errorf("the header line = %q, want %q", got, want)
	}
	if got, want := jevClassText(t, dom, "jvspend"), "$1.75 in 24h · top caller overseer (10 calls)"; got != want {
		t.Errorf("the spend line = %q, want %q", got, want)
	}
	if v, err := dom.vm.RunString(`__byId["jev"].textContent`); err != nil || !strings.Contains(v.String(), "the JEV summary could not be read: the groups are left out") {
		t.Errorf("the panel does not say the summary is missing: %v %q", err, v)
	}
}

// A scorecard that cannot be read says so on the JEV title line and draws no
// battery lines, and the Cost panel's providers table is left as it was.
func TestJevPanelFailureSaysUnavailableOnItsTitle(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// A fresh scorecard draws no alarm, so the alarms below are the read's own.
	_, fresh, _ := drawJev(t, jevScorecardState(now, map[string]any{}))
	if fresh.Class != "r" || strings.HasPrefix(fresh.Text, "unavailable") {
		t.Errorf("a fresh scorecard's title line = %q (class %q), want no alarm", fresh.Text, fresh.Class)
	}
	cases := map[string]struct {
		read  map[string]any
		want  string
		class string
	}{
		"failed": {map[string]any{"error": true, "stale": true}, "unavailable — the read failed", "r badc"},
		"stale":  {map[string]any{"stale": true}, "unavailable — the read went stale", "r warnc"},
	}
	for name, tc := range cases {
		rows, note, _ := drawJev(t, jevScorecardState(now, tc.read))
		if note.Text != tc.want || note.Class != tc.class {
			t.Errorf("%s: title line = %q (class %q), want %q (class %q)", name, note.Text, note.Class, tc.want, tc.class)
		}
		if len(rows) != 0 {
			t.Errorf("%s: drew %v from an unreadable scorecard", name, jevRowTexts(rows))
		}
	}

	// Never read: the failed status on the title line, no lines.
	state := jevStateAt(now, map[string]any{"reads": jevRead(nil)})
	state["reads"].(map[string]any)["jev_scorecard"] = map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "every_sec": 900, "error": true, "stale": true}
	_, note, sec := drawJev(t, state)
	if note.Text != "unavailable — the read failed" || sec.Hidden {
		t.Errorf("never-read title line = %q (hidden %v)", note.Text, sec.Hidden)
	}
}

// No value and no status leaves the panel out.
func TestJevPanelAbsentIsHidden(t *testing.T) {
	t.Parallel()

	_, _, sec := drawJev(t, jevState(map[string]any{"reads": jevRead(nil)}))
	if !sec.Hidden {
		t.Error("a missing scorecard shows the JEV panel")
	}
}

// A cache no refresh has written yet says how to make one and draws no battery
// rows, rather than reading as a scorecard with nothing in it.
func TestJevPanelMissingScorecardSaysHowToMakeOne(t *testing.T) {
	t.Parallel()

	now := time.Now()
	state := jevStateAt(now, map[string]any{
		"reads":         map[string]any{"jev_scorecard": map[string]any{"at": now.UTC().Format(time.RFC3339), "every_sec": 60}},
		"jev_scorecard": map[string]any{"no_scorecard": true},
	})
	rows, note, sec := drawJevAt(t, now, state)
	if want := "no scorecard yet: run gt jev scorecard --refresh"; note.Text != want {
		t.Errorf("title line = %q, want %q", note.Text, want)
	}
	if len(rows) != 0 {
		t.Errorf("a missing cache drew rows: %q", jevRowTexts(rows))
	}
	if sec.Hidden {
		t.Error("the panel is hidden over a missing cache")
	}
}

// Every battery line says how old its own score is: the age comes from the
// block's scored_at, not from the age of the read that carried it.
func TestJevPanelShowsEachBatterysScoreAge(t *testing.T) {
	t.Parallel()

	now := time.Now()
	rows, _, _ := drawJevAt(t, now, jevScorecardState(now, nil))
	names := map[string]string{}
	for _, r := range rows {
		if len(r.Cells[0].Kids) < 2 {
			t.Fatalf("row %q draws no score age", r.Cells[0].Text)
		}
		names[r.Cells[0].Kids[0].Text] = r.Cells[0].Kids[1].Text
	}
	for _, name := range []string{"finding", "landing", "targets"} {
		if got := names[name]; got != "scored 5h00m ago" {
			t.Errorf("%s's score age = %q, want %q", name, got, "scored 5h00m ago")
		}
	}
}

// A cache-kept block is the block the cache drew: the hub does not replace it
// with its older copy of the line.
func TestHubLeavesACacheKeptBlockAsTheReaderDrewIt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	scored := now.Add(-time.Hour)
	reads := []JevBattery{
		{Name: "finding", Items: 5, ScoredAt: &scored},
		{Name: "finding", Items: 9, Failed: true, Kept: true, ScoredAt: &scored},
	}
	next := 0
	h := NewHub(Config{
		Now: func() time.Time { return now },
		JevScorecard: func() *JevScorecard {
			sc := &JevScorecard{Batteries: []JevBattery{reads[next]}}
			next++
			sc.Tally()
			return sc
		},
	})
	h.pollJevScorecard()
	h.pollJevScorecard()
	got := h.State().JevScorecard.Batteries[0]
	if !got.Kept || got.Items != 9 {
		t.Errorf("after a kept block = %+v, want the cache's kept line", got)
	}
}

// A cache no refresh has written is a value, not a failed read: the page says
// how to make one and the read is not flagged.
func TestJevScorecardNoCacheIsNotAFailedRead(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{JevScorecard: func() *JevScorecard { return &JevScorecard{NoScorecard: true} }})
	h.pollJevScorecard()
	st := h.State()
	if st.JevScorecard == nil || !st.JevScorecard.NoScorecard {
		t.Errorf("a missing cache read %+v, want the no-scorecard value", st.JevScorecard)
	}
	if r := st.Reads[SourceJevScorecard]; r.Error || r.Stale {
		t.Errorf("a missing cache is flagged as a failed read: %+v", r)
	}
}

// The JEV panel sits after Tier sweeps, starts folded until it is opened, and
// is drawn on every render.
func TestJevPanelSitsAfterTierSweepsAndFoldsUntilOpened(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	at := strings.Index(page, `<section id="jevsec" data-fold="jev" data-default-collapsed hidden><h2>JEV · <span class="r" id="jevnote"></span></h2><div class="body" id="jev"></div></section>`)
	if at < 0 {
		t.Fatal("index.html has no JEV panel")
	}
	if tier := strings.Index(page, `<section data-fold="tiersweep"><h2><span class="sweepico" id="tiersweepico"></span>Tier sweeps`); tier < 0 || tier > at {
		t.Error("the JEV panel is not after Tier sweeps")
	}
	dom := newPageDOM(t)
	if !dom.fold(t, "jev").Collapsed {
		t.Error("the JEV pane starts open on first load")
	}
	dom.press(t, "jev", "click")
	if dom.fold(t, "jev").Collapsed {
		t.Error("the JEV pane does not open when its heading is clicked")
	}
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderJev(state)") {
		t.Error("renderAll does not draw the JEV panel")
	}
	if cost := pageFunc(t, "renderCost"); strings.Contains(cost, "costJevScorecard") {
		t.Error("renderCost still draws the scorecard lines")
	}
}

// Every class the page gives a verdict chip has a colour rule drawn from a
// token the page defines, so a verdict added to JV_CLASS without a rule, or with
// a rule naming a missing token, draws an uncoloured chip.
func TestJevChipClassesHaveColourRulesFromDefinedTokens(t *testing.T) {
	t.Parallel()

	v, err := newPageDOM(t).vm.RunString(`JSON.stringify(JV_CLASS)`)
	if err != nil {
		t.Fatalf("reading JV_CLASS: %v", err)
	}
	var classes map[string]string
	if err := json.Unmarshal([]byte(v.String()), &classes); err != nil {
		t.Fatalf("reading JV_CLASS: %v", err)
	}
	if len(classes) == 0 {
		t.Fatal("JV_CLASS names no verdict")
	}

	page := string(indexHTML)
	for verdict, class := range classes {
		rule := regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `\{color:var\((--[a-z-]+)\)\}`).FindStringSubmatch(page)
		if rule == nil {
			t.Errorf("verdict %q wears %q, which index.html's style gives no colour from a token", verdict, class)
			continue
		}
		if !strings.Contains(page, rule[1]+":#") {
			t.Errorf("verdict %q wears %q, whose colour %s the page never defines", verdict, class, rule[1])
		}
	}
}

// The title line's totals are the sums Tally makes over the batteries: lines
// and labeled lines, and the questions carrying each verdict.
func TestJevScorecardTallyCountsTheTitleLine(t *testing.T) {
	t.Parallel()

	sc := JevScorecard{Batteries: []JevBattery{
		{Name: "finding", Items: 212, Labeled: 140, Questions: []JevQuestion{{ID: "fix_worthy", Verdict: "earning"}, {ID: "any_duplicate", Verdict: "not-yet"}}},
		{Name: "landing"},
		{Name: "targets", Items: 9, Questions: []JevQuestion{{ID: "needs_review", Verdict: "no-signal"}, {ID: "noise", Verdict: "not-yet"}}},
	}}
	sc.Tally()
	if want := (JevTotals{Batteries: 3, Items: 221, Labeled: 140, Earning: 1, NoSignal: 1, NotYet: 2}); sc.Totals != want {
		t.Errorf("Totals = %+v, want %+v", sc.Totals, want)
	}
}

// The packages battery dwarfs the rest and is still no part of the title line's
// batteries, items or labeled count: its packages are not items. Its question
// verdicts count, since the chips count questions rather than volume.
func TestJevScorecardTallyLeavesThePackagesBatteryOut(t *testing.T) {
	t.Parallel()

	sc := JevScorecard{Batteries: []JevBattery{
		{Name: "rules", Items: 25, Labeled: 25, Unanswered: 53},
		{Name: JevPackagesBattery, Items: 39858, Labeled: 30000, Unit: "packages",
			Questions: []JevQuestion{{ID: "affects_tests", Verdict: "earning"}}},
	}}
	sc.Tally()
	if want := (JevTotals{Batteries: 1, Items: 25, Labeled: 25, Earning: 1}); sc.Totals != want {
		t.Errorf("Totals = %+v, want %+v", sc.Totals, want)
	}
}

// The state JSON carried a battery's count as lines before the panel called it
// items: /api/state keeps sending both names with the same number, so a consumer
// of the older key reads the count rather than an absent field.
func TestJevScorecardServesTheCountUnderItsOldNameToo(t *testing.T) {
	t.Parallel()

	sc := &JevScorecard{Batteries: []JevBattery{{Name: "finding", Items: 212, Labeled: 140}}}
	sc.Tally()
	b, err := json.Marshal(State{JevScorecard: sc})
	if err != nil {
		t.Fatal(err)
	}
	type count struct {
		Items *int `json:"items"`
		Lines *int `json:"lines"`
	}
	var got struct {
		Scorecard struct {
			Totals    count   `json:"totals"`
			Batteries []count `json:"batteries"`
		} `json:"jev_scorecard"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("the state JSON is not the page's shape: %v", err)
	}
	if len(got.Scorecard.Batteries) != 1 {
		t.Fatalf("the state JSON carries %d batteries, want the one: %s", len(got.Scorecard.Batteries), b)
	}
	for _, c := range []struct {
		what string
		c    count
	}{{"totals", got.Scorecard.Totals}, {"the finding battery", got.Scorecard.Batteries[0]}} {
		if c.c.Items == nil || c.c.Lines == nil {
			t.Errorf("%s carries items %v and lines %v, want both: %s", c.what, c.c.Items, c.c.Lines, b)
			continue
		}
		if *c.c.Lines != *c.c.Items {
			t.Errorf("%s says items %d but lines %d, want the same count", c.what, *c.c.Items, *c.c.Lines)
		}
	}
}

// The scorecard panel picks up a new score within a minute of its cache being
// written, so the default poll is a minute.
func TestJevScorecardEveryDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.JevScorecardEvery != time.Minute {
		t.Errorf("JevScorecardEvery defaults to %v, want 1m", c.JevScorecardEvery)
	}
}
