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

// ParseJevScorecard turns one battery's `gt jev scorecard --json` report into
// its line: the parsed items, the ones a rule labeled, the lines the report
// skipped as unanswered, and each question's verdict in the report's order. The
// packages battery says what its items are.
func TestParseJevScorecard(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		report string
		want   JevBattery
	}{
		{"questions and labels", `{"battery":"finding","items":3,"lines_skipped":1,
 "questions":[{"id":"any_duplicate","kind":"noul","logged":3,"labeled":2,"verdict":"not-yet"},
              {"id":"fix_worthy","kind":"noul","logged":3,"labeled":2,"verdict":"earning"}],
 "lines":[{"id":"a","labeled":true},{"id":"b","labeled":false},{"id":"c","labeled":true}]}`,
			JevBattery{Name: "finding", Items: 3, Labeled: 2, Unanswered: 1, Questions: []JevQuestion{
				{ID: "any_duplicate", Verdict: "not-yet", Labeled: 2}, {ID: "fix_worthy", Verdict: "earning", Labeled: 2}}}},
		{"the skipped lines are the unanswered count", `{"battery":"finding","items":7,"lines_skipped":3,"questions":[]}`,
			JevBattery{Name: "finding", Items: 7, Unanswered: 3}},
		{"the chip's figures", `{"battery":"landing","items":9,"labeled":7,
 "questions":[{"id":"first_try","kind":"noul","logged":9,"labeled":7,"positives":3,"negatives":4,"base_rate":0.25,"precision":0.625,"recall":0.5,"verdict":"earning"}]}`,
			JevBattery{Name: "landing", Items: 9, Labeled: 7, Questions: []JevQuestion{
				{ID: "first_try", Verdict: "earning", Labeled: 7, BaseRate: jevFloat(0.25), Precision: jevFloat(0.625)}}}},
		{"a null figure is absent", `{"battery":"drift","items":4,
 "questions":[{"id":"noise","kind":"noul","logged":4,"labeled":1,"base_rate":null,"precision":null,"recall":null,"verdict":"not-yet"}]}`,
			JevBattery{Name: "drift", Items: 4, Questions: []JevQuestion{{ID: "noise", Verdict: "not-yet", Labeled: 1}}}},
		{"the labeled total stands in for rows the report left out", `{"battery":"testsel","items":9000,"labeled":6000,"lines_omitted":true,
 "questions":[{"id":"affects_tests","verdict":"earning"}]}`,
			JevBattery{Name: "testsel", Items: 9000, Labeled: 6000, Unit: "packages", Questions: []JevQuestion{{ID: "affects_tests", Verdict: "earning"}}}},
		{"the labeled total is the one the report states", `{"battery":"finding","items":2,"labeled":1,
 "questions":[],"lines":[{"id":"a","labeled":true},{"id":"b","labeled":false}]}`,
			JevBattery{Name: "finding", Items: 2, Labeled: 1}},
		{"no log is an empty report", `{"battery":"landing","items":0,"lines_skipped":0,"questions":[]}`,
			JevBattery{Name: "landing"}},
		{"the text report's lines are optional", `{"battery":"dispatch","items":5,
 "questions":[{"id":"first_try","verdict":"no-signal"}]}`,
			JevBattery{Name: "dispatch", Items: 5, Questions: []JevQuestion{{ID: "first_try", Verdict: "no-signal"}}}},
	}
	for _, tc := range cases {
		got, err := ParseJevScorecard([]byte(tc.report))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, *got, tc.want)
		}
	}
}

func TestParseJevScorecardRefusesWhatIsNotTheReport(t *testing.T) {
	t.Parallel()

	for _, out := range []string{"", "   ", "gt: unknown battery", "[]", `{"items":3}`, `{"battery":"finding"}`, `{"battery":`} {
		if got, err := ParseJevScorecard([]byte(out)); err == nil {
			t.Errorf("ParseJevScorecard(%q) = %+v, want an error", out, got)
		}
	}
}

func TestJevScorecardWindowReadsTheCommandsSince(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"gt jev scorecard --json --since 7d": "7d",
		"gt jev scorecard --since=36h":       "36h",
		"gt jev scorecard --json":            "",
		"gt jev scorecard --since":           "",
	}
	for cmd, want := range cases {
		if got := JevScorecardWindow(strings.Fields(cmd)); got != want {
			t.Errorf("JevScorecardWindow(%q) = %q, want %q", cmd, got, want)
		}
	}
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

// jevFloat is a scorecard figure for a want value.
func jevFloat(v float64) *float64 { return &v }

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
	sc["batteries"] = []any{
		map[string]any{"name": "finding", "items": 212, "labeled": 140},
		map[string]any{"name": "dispatch", "items": 4, "labeled": 1, "failed": true, "kept": true},
		map[string]any{"name": "drift", "failed": true},
	}
	sc["unavailable"] = []any{"dispatch", "drift"}
	sc["totals"] = map[string]any{"batteries": 2, "items": 216, "labeled": 141}

	rows, note, _ := drawJevAt(t, now, s)
	got := jevRowTexts(rows)
	want := []string{
		"dispatch41 (stale: the read failed)",
		"drift--unavailable",
		"finding212140",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JEV rows = %q, want %q", got, want)
	}
	if wantNote := "2 batteries · 216 items · 141 labeled · last 7d · read 2m ago 0 earning 0 no-signal 0 not-yet · 2 unavailable: dispatch, drift"; note.Text != wantNote {
		t.Errorf("JEV title line = %q, want %q", note.Text, wantNote)
	}
	if note.Class != "r warnc" {
		t.Errorf("a title line naming lost batteries wears %q, want the amber class", note.Class)
	}
}

// A scorecard the state carries without a read record has no age to state, and
// the title line says so rather than leaving the question open.
func TestJevPanelSaysWhenTheReadsAgeIsUnknown(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevScorecardState(now, nil)
	delete(s["reads"].(map[string]any), "jev_scorecard")

	_, note, _ := drawJevAt(t, now, s)
	if want := "3 batteries · 221 items · 140 labeled · last 7d · read age unknown 1 earning 1 no-signal 1 not-yet"; note.Text != want {
		t.Errorf("JEV title line = %q, want %q", note.Text, want)
	}
}

// jevScorecardState is the JEV panel's read of three batteries: finding, with
// an earning and a not-yet question; landing, with no log; and targets, with
// one no-signal question. The totals are what Tally makes of them, which
// TestJevScorecardTallyCountsTheTitleLine pins.
func jevScorecardState(now time.Time, read map[string]any) map[string]any {
	over := map[string]any{
		"reads": jevRead(nil),
		"jev_scorecard": map[string]any{"window": "7d", "batteries": []any{
			map[string]any{"name": "finding", "items": 212, "labeled": 140, "questions": []any{
				map[string]any{"id": "fix_worthy", "verdict": "earning", "labeled": 140, "base_rate": 0.5, "precision": 0.75},
				map[string]any{"id": "any_duplicate", "verdict": "not-yet", "labeled": 40}}},
			map[string]any{"name": "landing", "items": 0, "labeled": 0},
			map[string]any{"name": "targets", "items": 9, "labeled": 0, "questions": []any{
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

// jevTableJS reads the panel's table back as rows of cells, each cell with what
// it holds. A panel that drew no table reads as no rows.
const jevTableJS = `JSON.stringify((function () {
  var box = __byId["jev"], t = box.children[0], rows = [];
  if (!t || t.tagName !== "TABLE") return rows;
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
  var t = __byId["jev"].children[0], out = [];
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
	v, err := dom.vm.RunString(`JSON.stringify(__byId["jevnote"].children.filter(c => c.className && c.className.indexOf("jvchip") === 0).map(c => ({tag: c.tagName, class: c.className, text: c.textContent, title: c.title})))`)
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
  var td = __byId["jev"].children[0].children[1].children[%d].children[3];
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
  var td = __byId["jev"].children[0].children[1].children[%d].children[3], btn = null, more = null;
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

	// The three verdicts are chips beside the totals, green, amber and dim.
	if got, want := (jevRowTexts(rows))[2], "targets90needs_review"; got != want {
		t.Errorf("targets' row = %q, want %q", got, want)
	}
	if wantNote := "3 batteries · 221 items · 140 labeled · last 7d · read 2m ago 1 earning 1 no-signal 1 not-yet"; note.Text != wantNote {
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
	if wantNote := "1 battery · 212 items · 140 labeled · last 7d · read 2m ago 1 earning 0 no-signal 0 not-yet"; note.Text != wantNote {
		t.Errorf("JEV title line = %q, want %q", note.Text, wantNote)
	}
	if strings.Contains(note.Text, "39858") {
		t.Errorf("the packages battery is counted in the title line: %q", note.Text)
	}
}

// The title line says which window the scorecard read and how old that read is,
// both off the data: a 36h scorecard says 36h, and nothing says 7d on its own.
func TestJevTitleLineStatesTheWindowAndReadAge(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := jevScorecardState(now, nil)
	s["jev_scorecard"].(map[string]any)["window"] = "36h"

	_, note, _ := drawJevAt(t, now, s)
	for _, want := range []string{"last 36h", "read 2m ago"} {
		if !strings.Contains(note.Text, want) {
			t.Errorf("the title line %q does not say %q", note.Text, want)
		}
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

// The title line's verdict counts are chips coloured from the page's tokens,
// and its tooltip says what each verdict means.
func TestJevTitleLineCountsTheVerdictsAsChips(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	_, note, _ := drawJevIn(t, dom, jevScorecardState(time.Now(), nil))
	chips := jevNoteChips(t, dom)
	want := []struct{ text, class, title string }{
		{"1 earning", "jvchip jv-earning", "enough labels and a signal"},
		{"1 no-signal", "jvchip jv-nosignal", "enough labels, nothing beyond the base rate"},
		{"1 not-yet", "jvchip jv-notyet", "under 30 labels"},
	}
	if len(chips) != len(want) {
		t.Fatalf("the title line drew %d chips, want %d: %+v", len(chips), len(want), chips)
	}
	for i, w := range want {
		if chips[i].Text != w.text || chips[i].Class != w.class || chips[i].Title != w.title {
			t.Errorf("title chip %d = %+v, want %q in %q saying %q", i, chips[i], w.text, w.class, w.title)
		}
	}
	for _, w := range want {
		if !strings.Contains(note.Title, w.title) {
			t.Errorf("the title line's tooltip %q does not say %q", note.Title, w.title)
		}
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

// No scorecard command, or one that has not answered yet, leaves the panel out.
func TestJevPanelAbsentIsHidden(t *testing.T) {
	t.Parallel()

	_, _, sec := drawJev(t, jevState(map[string]any{"reads": jevRead(nil)}))
	if !sec.Hidden {
		t.Error("a missing scorecard shows the JEV panel")
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
