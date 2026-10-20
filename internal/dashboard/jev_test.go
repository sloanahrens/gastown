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
	s := map[string]any{
		"spend":       map[string]any{"balance": 12.5, "live": map[string]any{"ok": true}},
		"jev_spend":   map[string]any{"calls": 14, "usd": 1.75, "window": "24h", "top_caller": "overseer", "top_caller_calls": 10, "top_caller_usd": 1.5},
		"claude_plan": claudePlan(time.Now(), nil),
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

	without, _ := drawCost(t, jevState(map[string]any{"jev_spend": nil}))
	failed, _ := drawCost(t, jevState(map[string]any{
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

func jevScorecardState(read map[string]any) map[string]any {
	over := map[string]any{
		"reads": jevRead(nil),
		"jev_scorecard": map[string]any{"window": "7d", "batteries": []any{
			map[string]any{"name": "finding", "lines": 212, "labeled": 140, "questions": []any{
				map[string]any{"id": "fix_worthy", "verdict": "earning"},
				map[string]any{"id": "any_duplicate", "verdict": "not-yet"}}},
			map[string]any{"name": "landing", "lines": 0, "labeled": 0},
			map[string]any{"name": "targets", "lines": 9, "labeled": 0, "questions": []any{
				map[string]any{"id": "needs_review", "verdict": "no-signal"}}},
		}},
	}
	reads := over["reads"].(map[string]any)
	reads["jev_scorecard"] = map[string]any{"at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "every_sec": 900}
	for k, v := range read {
		reads["jev_scorecard"].(map[string]any)[k] = v
	}
	return jevState(over)
}

// Golden: the spend row, then one line per battery, and a dash for the battery
// without a log.
func TestCostPanelDrawsOneJevLinePerBattery(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, jevScorecardState(nil))
	var got []string
	for _, l := range lines {
		if strings.HasPrefix(l.Text, "Jev spend") || strings.HasPrefix(l.Text, "finding") ||
			strings.HasPrefix(l.Text, "landing") || strings.HasPrefix(l.Text, "targets") {
			got = append(got, l.Text)
		}
	}
	want := []string{
		"Jev spend · 14 calls in 24h · $1.75 · top caller overseer (10 calls, $1.50)",
		"finding 212/140 fix_worthy:earning any_duplicate:not-yet",
		"landing -",
		"targets 9/0 needs_review:no-signal",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Jev lines = %q, want %q", got, want)
	}
}

// A scorecard that cannot be read says so on one line and leaves the spend
// line as it was.
func TestCostPanelJevScorecardFailureKeepsTheSpendLine(t *testing.T) {
	t.Parallel()

	healthy, _ := drawCost(t, jevState(map[string]any{"reads": jevRead(nil)}))
	spend, _ := findLine(healthy, "Jev spend")

	cases := map[string]struct {
		read map[string]any
		want string
	}{
		"failed": {map[string]any{"error": true, "stale": true}, "Jev scorecard unavailable — the read failed"},
		"stale":  {map[string]any{"stale": true}, "Jev scorecard unavailable — the read went stale"},
	}
	for name, tc := range cases {
		lines, _ := drawCost(t, jevScorecardState(tc.read))
		row, ok := findLine(lines, "Jev scorecard")
		if !ok || row.Text != tc.want || row.Class != "empty" {
			t.Errorf("%s: row = %+v (found %v), want %q", name, row, ok, tc.want)
		}
		if got, _ := findLine(lines, "Jev spend"); got != spend {
			t.Errorf("%s: spend row = %+v, want %+v", name, got, spend)
		}
		if _, ok := findLine(lines, "finding"); ok {
			t.Errorf("%s: a battery line is drawn from an unreadable scorecard", name)
		}
	}

	// Never read: only the failed status, no lines.
	state := jevState(map[string]any{"reads": jevRead(nil)})
	state["reads"].(map[string]any)["jev_scorecard"] = map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "every_sec": 900, "error": true, "stale": true}
	lines, _ := drawCost(t, state)
	if row, ok := findLine(lines, "Jev scorecard"); !ok || row.Text != "Jev scorecard unavailable — the read failed" {
		t.Errorf("never-read row = %+v (found %v)", row, ok)
	}
}

// No scorecard command, or one that has not answered yet, draws nothing.
func TestCostPanelJevScorecardAbsentDrawsNothing(t *testing.T) {
	t.Parallel()

	lines, _ := drawCost(t, jevState(map[string]any{"reads": jevRead(nil)}))
	if row, ok := findLine(lines, "Jev scorecard"); ok {
		t.Errorf("a missing scorecard drew %+v", row)
	}
}
