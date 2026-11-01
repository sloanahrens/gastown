package metrics

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// Unit tier: pure functions over in-memory bytes, no files, no clock.

func TestQuestionReviewLoopCountsRoundsPerBead(t *testing.T) {
	t.Parallel()
	reviews := []byte(strings.Join([]string{
		`{"bead":"gt-a","attempt":1,"at":"2026-11-01T01:00:00Z"}`,
		`{"bead":"gt-b","attempt":1,"at":"2026-11-01T02:00:00Z"}`,
		`{"bead":"gt-b","attempt":2,"at":"2026-11-01T03:00:00Z"}`,
		`{"bead":"gt-c","attempt":1,"at":"2026-11-01T04:00:00Z"}`,
		`{"bead":"gt-c","attempt":3,"at":"2026-11-01T05:00:00Z"}`,
		`{"bead":"gt-old","attempt":4,"at":"2026-10-01T05:00:00Z"}`,
		`not json`,
	}, "\n"))
	q := QuestionReviewLoop(reviews, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	// gt-a 1 round, gt-b 2, gt-c 3; gt-old is before the window.
	for _, want := range []string{"3 beads reviewed", "mean 2.00 rounds", "67% need more than one round", "33% need three or more", "1 unparsed lines"} {
		if !strings.Contains(q.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, q.Detail)
		}
	}
	if q.Status != QuestionHeld || q.N != 3 {
		t.Errorf("status %q n %d, want held 3", q.Status, q.N)
	}
}

func TestQuestionReviewLoopWithNoRecordsInTheWindowIsMissing(t *testing.T) {
	t.Parallel()
	q := QuestionReviewLoop([]byte(`{"bead":"gt-old","attempt":1,"at":"2026-10-01T05:00:00Z"}`), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	if q.Status != QuestionMissing {
		t.Errorf("status %q, want missing; detail: %s", q.Status, q.Detail)
	}
}

func questionEventsFixture() []byte {
	return []byte(strings.Join([]string{
		// gt-a: created 10:00, submitted 12:00, closed 12:30 -> 2.0 / 0.5 / 2.5 h.
		`{"issue_id":"gt-a","event_type":"created","created_at":"2026-11-01 10:00:00"}`,
		`{"issue_id":"gt-a","event_type":"label_added","comment":"Added label: gt:ready-to-land","created_at":"2026-11-01 12:00:00"}`,
		`{"issue_id":"gt-a","event_type":"label_added","comment":"Added label: gt:bug","created_at":"2026-11-01 10:05:00"}`,
		`{"issue_id":"gt-a","event_type":"closed","created_at":"2026-11-01 12:30:00"}`,
		// gt-b: created 08:00, submitted 14:00, closed 14:12 -> 6.0 / 0.2 / 6.2 h.
		`{"issue_id":"gt-b","event_type":"created","created_at":"2026-11-01 08:00:00"}`,
		`{"issue_id":"gt-b","event_type":"label_added","comment":"Added label: gt:ready-to-land","created_at":"2026-11-01 14:00:00"}`,
		`{"issue_id":"gt-b","event_type":"closed","created_at":"2026-11-01 14:12:00"}`,
		// gt-c: no submit stamp.
		`{"issue_id":"gt-c","event_type":"created","created_at":"2026-11-01 09:00:00"}`,
		`{"issue_id":"gt-c","event_type":"closed","created_at":"2026-11-01 09:30:00"}`,
		`garbage`,
	}, "\n"))
}

func TestQuestionTimeToLandSplitsTheStagesAndCountsMissingStamps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	landed := []LandedBead{
		{"gt-a", time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)},
		{"gt-b", time.Date(2026, 11, 1, 14, 12, 0, 0, time.UTC)},
		{"gt-c", time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)},
		{"gt-old", time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)},
	}
	q := QuestionTimeToLand(questionEventsFixture(), landed, now, 7)
	// Median of (2.0, 6.0) = 4.0 to submit; of (0.5, 0.2) = 0.35 to close; of (2.5, 6.2) = 4.35 total.
	for _, want := range []string{"11-01 4.0/0.35/4.3", "n=2", "1 of 3 landings lacked a stage stamp", "1 unparsed event lines"} {
		if !strings.Contains(q.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, q.Detail)
		}
	}
}

func TestQuestionTimeToLandWithoutStampsIsMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	q := QuestionTimeToLand(nil, []LandedBead{{"gt-a", now.Add(-time.Hour)}}, now, 7)
	if q.Status != QuestionMissing || !strings.Contains(q.Detail, "1 landings, 1 without a stamp") {
		t.Errorf("want missing with the counts, got %q: %s", q.Status, q.Detail)
	}
}

func TestParseLedgerLandingsKeepsDaemonRouteOfThePrefix(t *testing.T) {
	t.Parallel()
	csvData := []byte("bead,rig,landed_at,route\n" +
		"gt-a,gastown,2026-11-01T12:30:00Z,daemon\n" +
		"gt-b,gastown,2026-11-01T13:30:00.5Z,overseer-manual\n" +
		"dv-a,devops,2026-11-01T12:30:00Z,daemon\n" +
		"gt-c,gastown,not-a-time,daemon\n")
	got, bad, err := ParseLedgerLandings(csvData, "gt-")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Bead != "gt-a" || bad != 1 {
		t.Errorf("got %+v bad %d, want only gt-a and one bad row", got, bad)
	}
	if _, _, err := ParseLedgerLandings([]byte("bead,landed_at\ngt-a,x\n"), "gt-"); err == nil {
		t.Error("a ledger without a route column must be an error")
	}
}

const questionSnapshotFixture = `{"ts":"2026-11-01T16:13:49Z","host":{"load1":13.51,"cpu_idle_pct":25.47,"bd_latency_ms":195.97,"dolt_conns":35},` +
	`"queue":{"ready":215,"blocked":60,"dispatchable":28,"waiting_to_land":0,"last_tick_secs":11.5,"seat_live_sbx-flash":7,"seat_live_sbx-haiku":0,"seat_cap_sbx-flash":8},` +
	`"money":{"deepseek_per_hour":1.497,"deepseek_balance":5.2,"deepseek_spend_age_s":1519621,"claude_plan_weekly_pct":70,"claude_plan_weekly_pace_per_day_1h":22.12}}`

func TestQuestionSaturationReadsTheLastSnapshot(t *testing.T) {
	t.Parallel()
	q := QuestionSaturation([]byte(questionSnapshotFixture))
	for _, want := range []string{"load1 13.5", "cpu idle 25%", "bd latency 196 ms", "dolt connections 35", "live seats 7", "tick 11.5 s"} {
		if !strings.Contains(q.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, q.Detail)
		}
	}
	if got := QuestionSaturation([]byte("{")); got.Status != QuestionMissing {
		t.Errorf("a snapshot that does not parse must be missing, got %q", got.Status)
	}
}

func TestQuestionSpendCallsAStaleBalanceUnknown(t *testing.T) {
	t.Parallel()
	q := QuestionSpend([]byte(questionSnapshotFixture))
	if !strings.Contains(q.Detail, "balance unknown (the reading is 17.6 days old)") {
		t.Errorf("a 17.6 day old balance must read unknown:\n%s", q.Detail)
	}
	if strings.Contains(q.Detail, "$5.20") {
		t.Errorf("the stale balance must not be printed:\n%s", q.Detail)
	}
	fresh := strings.Replace(questionSnapshotFixture, `"deepseek_spend_age_s":1519621`, `"deepseek_spend_age_s":60`, 1)
	if q := QuestionSpend([]byte(fresh)); !strings.Contains(q.Detail, "balance $5.20") {
		t.Errorf("a fresh balance must print:\n%s", q.Detail)
	}
}

func TestQuestionBacklogReadsTheQueueGroup(t *testing.T) {
	t.Parallel()
	q := QuestionBacklog([]byte(questionSnapshotFixture))
	for _, want := range []string{"ready 215", "blocked 60", "dispatchable 28", "waiting to land 0"} {
		if !strings.Contains(q.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, q.Detail)
		}
	}
}

func TestLastLineSkipsTrailingBlankLines(t *testing.T) {
	t.Parallel()
	if got := string(LastLine([]byte("a\nb\n\n  \n"))); got != "b" {
		t.Errorf("LastLine = %q, want b", got)
	}
	if LastLine(nil) != nil {
		t.Error("LastLine(nil) must be nil")
	}
}

func TestRenderQuestionsListsAllTenInOrder(t *testing.T) {
	t.Parallel()
	qs := QuestionsNotComputed()
	qs = append(qs,
		Question{3, "Review loop", QuestionHeld, "d3"}, Question{4, "Time to land", QuestionHeld, "d4"},
		Question{6, "Saturation", QuestionHeld, "d6"}, Question{8, "Spend", QuestionPartial, "d8"},
		Question{9, "Backlog", QuestionPartial, "d9"})
	var buf bytes.Buffer
	if err := RenderQuestions(&buf, qs); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if len(line) > 3 && line[3] == '.' || len(line) > 2 && line[2] == '.' {
			order = append(order, strings.TrimSpace(strings.SplitN(line, ".", 2)[0]))
		}
	}
	if got := strings.Join(order, ","); got != "1,2,3,4,5,6,7,8,9,10" {
		t.Errorf("question order %q, want 1..10:\n%s", got, buf.String())
	}
}

func TestParseLedgerLandingsOfAnEmptyFileIsAnError(t *testing.T) {
	t.Parallel()
	if _, _, err := ParseLedgerLandings(nil, "gt-"); err == nil || !strings.Contains(err.Error(), "ledger header") {
		t.Errorf("an empty ledger must be a header error, got %v", err)
	}
}

type questionsRefusingWriter struct{}

func (questionsRefusingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func TestRenderQuestionsReturnsAWriteError(t *testing.T) {
	t.Parallel()
	err := RenderQuestions(questionsRefusingWriter{}, QuestionsNotComputed())
	if err == nil || !strings.Contains(err.Error(), "write refused") {
		t.Errorf("a refused write must be returned, got %v", err)
	}
}
