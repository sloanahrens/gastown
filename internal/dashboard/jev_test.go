package dashboard

import (
	"encoding/json"
	"reflect"
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

func costTexts(lines []costLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

func findLine(lines []costLine, prefix string) (costLine, bool) {
	for _, l := range lines {
		if strings.HasPrefix(l.Text, prefix) {
			return l, true
		}
	}
	return costLine{}, false
}

// The Jev row sits between the Claude plan line and the rest, with the calls,
// the dollars and the top caller read off the report.
func TestCostPanelDrawsTheJevRowBesideDeepSeekAndClaude(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, jevState(map[string]any{"reads": jevRead(nil)}))
	row, ok := findLine(lines, "Jev spend")
	if !ok {
		t.Fatalf("no Jev row in %q", costTexts(lines))
	}
	if want := "Jev spend · 14 calls in 24h · $1.75 · top caller overseer (10 calls, $1.50)"; row.Text != want {
		t.Errorf("Jev row = %q, want %q", row.Text, want)
	}
	if _, ok := findLine(lines, "Claude plan"); !ok {
		t.Errorf("the Claude plan line went missing: %q", costTexts(lines))
	}
	var plan, jev = -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l.Text, "Claude plan"):
			plan = i
		case strings.HasPrefix(l.Text, "Jev spend"):
			jev = i
		}
	}
	if jev != plan+1 {
		t.Errorf("the Jev row is line %d, want it directly under the Claude plan line (%d)", jev, plan)
	}
}

func TestCostPanelJevRowWordsASingleCallAndNoCallers(t *testing.T) {
	t.Parallel()

	one := map[string]any{"calls": 1, "usd": 0.1, "top_caller": "a", "top_caller_calls": 1, "top_caller_usd": 0.1}
	lines, _ := drawCost(t, jevState(map[string]any{"jev_spend": one, "reads": jevRead(nil)}))
	if row, _ := findLine(lines, "Jev spend"); row.Text != "Jev spend · 1 call · $0.10 · top caller a (1 call, $0.10)" {
		t.Errorf("single-call row = %q", row.Text)
	}

	none := map[string]any{"calls": 0, "usd": 0}
	lines, _ = drawCost(t, jevState(map[string]any{"jev_spend": none, "reads": jevRead(nil)}))
	if row, _ := findLine(lines, "Jev spend"); row.Text != "Jev spend · 0 calls · $0.00" {
		t.Errorf("empty-window row = %q", row.Text)
	}
}

// A Jev command that never worked is "unavailable", and the DeepSeek and Claude
// lines are drawn exactly as they were without it.
func TestCostPanelJevFailureIsUnavailableAndLeavesTheOtherRows(t *testing.T) {
	t.Parallel()

	checkJevFailureLeavesTheOtherRows(t, time.Now())
}

// checkJevFailureLeavesTheOtherRows draws the panel with and without a failed Jev
// read, both from one now, and compares everything but the Jev row.
func checkJevFailureLeavesTheOtherRows(t *testing.T, now time.Time) {
	t.Helper()

	without, _ := drawCost(t, jevStateAt(now, map[string]any{"jev_spend": nil}))
	failed, _ := drawCost(t, jevStateAt(now, map[string]any{
		"jev_spend": nil,
		"reads":     jevRead(map[string]any{"error": true, "stale": true}),
	}))
	row, ok := findLine(failed, "Jev spend")
	if !ok {
		t.Fatalf("no Jev row in %q", costTexts(failed))
	}
	if row.Text != "Jev spend · unavailable — the read failed" || row.Class != "empty" {
		t.Errorf("failed row = %+v", row)
	}
	var rest []costLine
	for _, l := range failed {
		if !strings.HasPrefix(l.Text, "Jev spend") {
			rest = append(rest, l)
		}
	}
	if len(rest) != len(without) {
		t.Fatalf("the failed row changed the other rows: %q vs %q", costTexts(rest), costTexts(without))
	}
	for i := range rest {
		if rest[i] != without[i] {
			t.Errorf("row %d changed: %+v vs %+v", i, rest[i], without[i])
		}
	}
}

// The Claude plan line reads its reset time off the clock, and the reset moves
// at a minute boundary. Two builds either side of one would differ on that line,
// so the failed-row check would fail on the rows it leaves alone. Both draws of
// the check take one instant, so the check holds on the boundary itself.
func TestCostPanelJevFailureRowsAgreeAcrossAMinuteBoundary(t *testing.T) {
	t.Parallel()

	boundary := time.Date(2026, 10, 20, 12, 30, 0, 0, time.Local)
	before, after := boundary.Add(-time.Millisecond), boundary.Add(time.Millisecond)

	drawnBefore, _ := drawCost(t, jevStateAt(before, nil))
	drawnAfter, _ := drawCost(t, jevStateAt(after, nil))
	planBefore, _ := findLine(drawnBefore, "Claude plan")
	planAfter, _ := findLine(drawnAfter, "Claude plan")
	if planBefore.Text == planAfter.Text {
		t.Fatalf("the plan line reads %q on both sides of the boundary, so the boundary proves nothing", planBefore.Text)
	}

	checkJevFailureLeavesTheOtherRows(t, before)
	checkJevFailureLeavesTheOtherRows(t, after)
}

// A failed read after a good one says unavailable and keeps the last figures.
func TestCostPanelJevFailureAfterAGoodReadKeepsTheLastFigures(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, jevState(map[string]any{"reads": jevRead(map[string]any{"error": true, "stale": true})}))
	row, ok := findLine(lines, "Jev spend")
	if !ok {
		t.Fatalf("no Jev row in %q", costTexts(lines))
	}
	for _, want := range []string{"unavailable — the read failed", "last good", "14 calls in 24h", "$1.75"} {
		if !strings.Contains(row.Text, want) {
			t.Errorf("row %q has no %q", row.Text, want)
		}
	}
	if row.Class != "empty" {
		t.Errorf("a failed row wears %q, want the dimmed empty class", row.Class)
	}
}

// A read that answered but has gone stale says so, not that it failed, and
// keeps the last figures the same way.
func TestCostPanelJevStaleReadSaysStaleAndKeepsTheLastFigures(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, jevState(map[string]any{"reads": jevRead(map[string]any{"stale": true})}))
	row, ok := findLine(lines, "Jev spend")
	if !ok {
		t.Fatalf("no Jev row in %q", costTexts(lines))
	}
	for _, want := range []string{"unavailable — the read went stale", "last good", "14 calls in 24h", "$1.75"} {
		if !strings.Contains(row.Text, want) {
			t.Errorf("row %q has no %q", row.Text, want)
		}
	}
	if strings.Contains(row.Text, "the read failed") {
		t.Errorf("a stale read is worded as a failure: %q", row.Text)
	}
	if row.Class != "empty" {
		t.Errorf("a stale row wears %q, want the dimmed empty class", row.Class)
	}
}

// With no Jev command configured there is no reader and no status, and the
// panel draws no row at all, even with the rest of it empty.
func TestCostPanelDrawsNoJevRowWithoutAReader(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, jevState(map[string]any{"jev_spend": nil}))
	if row, ok := findLine(lines, "Jev spend"); ok {
		t.Errorf("a town with no Jev command drew %+v", row)
	}
}

// The row is drawn on the panel's one-line empty path too: its own reader owes
// the panel nothing.
func TestCostPanelJevRowDrawsWithoutASpendReport(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, map[string]any{
		"jev_spend": map[string]any{"calls": 2, "usd": 0.2},
		"reads":     jevRead(nil),
	})
	if len(lines) != 2 || !strings.Contains(lines[0].Text, "no spend report") || !strings.HasPrefix(lines[1].Text, "Jev spend · 2 calls · $0.20") {
		t.Errorf("empty panel drew %q", costTexts(lines))
	}
}

// ParseJevScorecard turns one battery's `gt jev scorecard --json` report into
// its line: the parsed items, the ones a rule labeled, and each question's
// verdict in the report's order.
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
			JevBattery{Name: "finding", Lines: 3, Labeled: 2, Questions: []JevQuestion{
				{ID: "any_duplicate", Verdict: "not-yet"}, {ID: "fix_worthy", Verdict: "earning"}}}},
		{"the labeled total stands in for rows the report left out", `{"battery":"testsel","items":9000,"labeled":6000,"lines_omitted":true,
 "questions":[{"id":"affects_tests","verdict":"earning"}]}`,
			JevBattery{Name: "testsel", Lines: 9000, Labeled: 6000, Questions: []JevQuestion{{ID: "affects_tests", Verdict: "earning"}}}},
		{"the labeled total is the one the report states", `{"battery":"finding","items":2,"labeled":1,
 "questions":[],"lines":[{"id":"a","labeled":true},{"id":"b","labeled":false}]}`,
			JevBattery{Name: "finding", Lines: 2, Labeled: 1}},
		{"no log is an empty report", `{"battery":"landing","items":0,"lines_skipped":0,"questions":[]}`,
			JevBattery{Name: "landing"}},
		{"the text report's lines are optional", `{"battery":"dispatch","items":5,
 "questions":[{"id":"first_try","verdict":"no-signal"}]}`,
			JevBattery{Name: "dispatch", Lines: 5, Questions: []JevQuestion{{ID: "first_try", Verdict: "no-signal"}}}},
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
			return &JevScorecard{Batteries: []JevBattery{{Name: "finding", Lines: 2, Labeled: 1}}}
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
		{{Name: "finding", Lines: 2, Labeled: 1}, {Name: "landing", Lines: 5}},
		{{Name: "finding", Lines: 3, Labeled: 2}, {Name: "landing", Failed: true}},
		{{Name: "finding", Failed: true}, {Name: "landing", Lines: 7}},
		{{Name: "finding", Lines: 4}, {Name: "landing", Lines: 8}},
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
	if want := (JevBattery{Name: "landing", Lines: 5, Failed: true, Kept: true}); !reflect.DeepEqual(got[1], want) {
		t.Errorf("landing after its failed read = %+v, want its last good line %+v", got[1], want)
	}
	if got[0].Lines != 3 || got[0].Failed {
		t.Errorf("finding was touched by landing's failed read: %+v", got[0])
	}
	if sc := h.State().JevScorecard; !reflect.DeepEqual(sc.Unavailable, []string{"landing"}) || sc.Totals.Lines != 8 || sc.Totals.Batteries != 2 {
		t.Errorf("scorecard = %+v, want landing named unavailable and its kept line counted", sc)
	}

	got = poll()
	if got[0].Lines != 3 || got[0].Labeled != 2 || !got[0].Kept || got[1].Lines != 7 || got[1].Failed {
		t.Errorf("after finding's failed read: %+v", got)
	}

	got = poll()
	if got[0].Lines != 4 || got[0].Failed || got[1].Lines != 8 || got[1].Failed {
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
			sc := &JevScorecard{Batteries: []JevBattery{{Name: "landing", Lines: 5}}}
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
	if got := poll(); !got.Kept || got.Lines != 5 {
		t.Errorf("a line exactly at its bound = %+v, want it kept", got)
	}
	now = now.Add(time.Second)
	if got := poll(); got.Kept || got.Lines != 0 || !got.Failed {
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
		sc := &JevScorecard{Batteries: []JevBattery{{Name: "finding", Lines: 1}, {Name: "landing", Failed: true}}}
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

// The JEV panel draws the batteries a partial read kept, names the lost ones on
// the title line, and marks a lost line stale or unavailable.
func TestJevPanelNamesTheBatteriesTheReadLost(t *testing.T) {
	t.Parallel()

	s := jevScorecardState(time.Now(), nil)
	sc := s["jev_scorecard"].(map[string]any)
	sc["batteries"] = []any{
		map[string]any{"name": "finding", "lines": 212, "labeled": 140},
		map[string]any{"name": "dispatch", "lines": 4, "labeled": 1, "failed": true, "kept": true},
		map[string]any{"name": "drift", "failed": true},
	}
	sc["unavailable"] = []any{"dispatch", "drift"}
	sc["totals"] = map[string]any{"batteries": 2, "lines": 216, "labeled": 141}

	lines, note, _ := drawJev(t, s)
	var got []string
	for _, l := range lines {
		got = append(got, l.Text)
	}
	want := []string{
		"finding 212 lines 140 labeled",
		"dispatch 4 lines 1 labeled (stale: the read failed)",
		"drift unavailable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JEV lines = %q, want %q", got, want)
	}
	if wantNote := "2 batteries · 216 lines · 141 labeled · 0 earning · 0 no-signal · 2 unavailable: dispatch, drift"; note.Text != wantNote {
		t.Errorf("JEV title line = %q, want %q", note.Text, wantNote)
	}
}

// jevScorecardState is the JEV panel's read of three batteries: finding, with
// two questions; landing, with no log; and targets, with one question. The
// totals are what Tally makes of them, which TestJevScorecardTallyCountsTheTitleLine
// pins.
func jevScorecardState(now time.Time, read map[string]any) map[string]any {
	over := map[string]any{
		"reads": jevRead(nil),
		"jev_scorecard": map[string]any{"window": "7d", "batteries": []any{
			map[string]any{"name": "finding", "lines": 212, "labeled": 140, "questions": []any{
				map[string]any{"id": "fix_worthy", "verdict": "earning"},
				map[string]any{"id": "any_duplicate", "verdict": "not-yet"}}},
			map[string]any{"name": "landing", "lines": 0, "labeled": 0},
			map[string]any{"name": "targets", "lines": 9, "labeled": 0, "questions": []any{
				map[string]any{"id": "needs_review", "verdict": "no-signal"}}},
		}, "totals": map[string]any{"batteries": 3, "lines": 221, "labeled": 140, "earning": 1, "no_signal": 1}},
	}
	reads := over["reads"].(map[string]any)
	reads["jev_scorecard"] = map[string]any{"at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "every_sec": 900}
	for k, v := range read {
		reads["jev_scorecard"].(map[string]any)[k] = v
	}
	return jevStateAt(now, over)
}

// drawJev runs renderJev over s and reads back the JEV panel's lines, its title
// line and the panel itself.
func drawJev(t *testing.T, s map[string]any) (lines []costLine, note, sec drawn) {
	t.Helper()
	dom := newPageDOM(t)
	dom.call(t, "renderJev", s)
	v, err := dom.vm.RunString(`JSON.stringify(__byId["jev"].children.map(c => ({class: c.className, text: c.textContent, title: c.title})))`)
	if err != nil {
		t.Fatalf("reading the JEV panel: %v", err)
	}
	if err := json.Unmarshal([]byte(v.String()), &lines); err != nil {
		t.Fatalf("reading the JEV panel: %v", err)
	}
	return lines, dom.element(t, "jevnote"), dom.element(t, "jevsec")
}

// The JEV panel draws one line per battery, and its title line the totals over
// all of them.
func TestJevPanelDrawsOneLinePerBatteryUnderItsTotals(t *testing.T) {
	t.Parallel()

	lines, note, sec := drawJev(t, jevScorecardState(time.Now(), nil))
	if sec.Hidden {
		t.Error("the JEV panel is hidden under a readable scorecard")
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.Text)
	}
	want := []string{
		"finding 212 lines 140 labeled fix_worthy:earning any_duplicate:not-yet",
		"landing -",
		"targets 9 lines 0 labeled needs_review:no-signal",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JEV lines = %q, want %q", got, want)
	}
	if wantNote := "3 batteries · 221 lines · 140 labeled · 1 earning · 1 no-signal"; note.Text != wantNote {
		t.Errorf("JEV title line = %q, want %q", note.Text, wantNote)
	}
}

// The Cost panel keeps the Jev spend row and draws none of the scorecard.
func TestCostPanelKeepsOnlyTheJevSpendRow(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, jevScorecardState(time.Now(), nil))
	if _, ok := findLine(lines, "Jev spend"); !ok {
		t.Fatalf("no Jev spend row in %q", costTexts(lines))
	}
	for _, prefix := range []string{"finding", "landing", "targets", "Jev scorecard"} {
		if row, ok := findLine(lines, prefix); ok {
			t.Errorf("the Cost panel draws a scorecard line %+v", row)
		}
	}
}

// A scorecard that cannot be read says so on the JEV title line and draws no
// battery lines, and the Cost panel's spend row is left as it was.
func TestJevPanelFailureSaysUnavailableOnItsTitle(t *testing.T) {
	t.Parallel()

	now := time.Now()
	healthy, _ := drawCost(t, jevStateAt(now, map[string]any{"reads": jevRead(nil)}))
	spend, _ := findLine(healthy, "Jev spend")

	cases := map[string]struct {
		read  map[string]any
		want  string
		class string
	}{
		"failed": {map[string]any{"error": true, "stale": true}, "unavailable — the read failed", "r badc"},
		"stale":  {map[string]any{"stale": true}, "unavailable — the read went stale", "r warnc"},
	}
	for name, tc := range cases {
		lines, note, _ := drawJev(t, jevScorecardState(now, tc.read))
		if note.Text != tc.want || note.Class != tc.class {
			t.Errorf("%s: title line = %q (class %q), want %q (class %q)", name, note.Text, note.Class, tc.want, tc.class)
		}
		if len(lines) != 0 {
			t.Errorf("%s: drew %q from an unreadable scorecard", name, costTexts(lines))
		}
		cost, _ := drawCost(t, jevScorecardState(now, tc.read))
		if got, _ := findLine(cost, "Jev spend"); got != spend {
			t.Errorf("%s: spend row = %+v, want %+v", name, got, spend)
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
	at := strings.Index(page, `<section id="jevsec" data-default-collapsed hidden><h2>JEV · <span class="r" id="jevnote"></span></h2><div class="body" id="jev"></div></section>`)
	if at < 0 {
		t.Fatal("index.html has no JEV panel")
	}
	if tier := strings.Index(page, `<section><h2><span class="sweepico" id="tiersweepico"></span>Tier sweeps`); tier < 0 || tier > at {
		t.Error("the JEV panel is not after Tier sweeps")
	}
	if setup := pageFunc(t, "setupCollapse"); !strings.Contains(setup, `apply(saved[name] === undefined ? sec.hasAttribute("data-default-collapsed") : !!saved[name]);`) {
		t.Error("setupCollapse does not fold a data-default-collapsed pane until it has been opened")
	}
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderJev(state)") {
		t.Error("renderAll does not draw the JEV panel")
	}
	if cost := pageFunc(t, "renderCost"); strings.Contains(cost, "costJevScorecard") {
		t.Error("renderCost still draws the scorecard lines")
	}
}

// The title line's totals are the sums Tally makes over the batteries: lines
// and labeled lines, and the questions earning and no-signal.
func TestJevScorecardTallyCountsTheTitleLine(t *testing.T) {
	t.Parallel()

	sc := JevScorecard{Batteries: []JevBattery{
		{Name: "finding", Lines: 212, Labeled: 140, Questions: []JevQuestion{{ID: "fix_worthy", Verdict: "earning"}, {ID: "any_duplicate", Verdict: "not-yet"}}},
		{Name: "landing"},
		{Name: "targets", Lines: 9, Questions: []JevQuestion{{ID: "needs_review", Verdict: "no-signal"}}},
	}}
	sc.Tally()
	if want := (JevTotals{Batteries: 3, Lines: 221, Labeled: 140, Earning: 1, NoSignal: 1}); sc.Totals != want {
		t.Errorf("Totals = %+v, want %+v", sc.Totals, want)
	}
}
