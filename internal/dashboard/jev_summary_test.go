package dashboard

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// jevSummaryFloat is the float a summary figure holds, failing the test when
// the figure is absent.
func jevSummaryFloat(t *testing.T, name string, got *float64) float64 {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is absent, want a figure", name)
	}
	return *got
}

// jevSummaryClose is whether two fractions agree to float precision.
func jevSummaryClose(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// jevSummaryScored is a scorecard whose refresh ran at now, with one battery
// line per block.
func jevSummaryScored(now time.Time, batteries ...JevBattery) *JevScorecard {
	sc := &JevScorecard{Batteries: batteries, ScoredAt: &now}
	sc.Tally()
	return sc
}

// jevSummaryQuestion is a question of a battery's line.
func jevSummaryQuestion(id, verdict string, labeled, positives, negatives int) JevQuestion {
	return JevQuestion{ID: id, Verdict: verdict, Kind: "noul", Labeled: labeled, Positives: positives, Negatives: negatives}
}

func TestJevSummaryListsEarningQuestionsWithLiftAndConsumer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	attention := jevSummaryQuestion("needs_human", "earning", 60, 20, 40)
	attention.BaseRate, attention.PrecisionAtBest = ptrFloat(0.25), ptrFloat(0.6)
	landing := jevSummaryQuestion("risk", "earning", 80, 30, 50)
	landing.BaseRate, landing.PrecisionAtBest = ptrFloat(0.4), ptrFloat(0.55)
	finding := jevSummaryQuestion("fix_worthy", "earning", 140, 70, 70)
	finding.BaseRate, finding.PrecisionAtBest = ptrFloat(0.5), ptrFloat(0.75)
	progress := JevQuestion{ID: "looping", Verdict: "earning", Kind: "score", Labeled: 50, Positives: 25, Negatives: 25, BaseRate: ptrFloat(0.5)}
	watching := jevSummaryQuestion("any_duplicate", "not-yet", 40, 4, 36)

	sc := jevSummaryScored(now,
		JevBattery{Name: "attention", Items: 100, Labeled: 60, ScoredAt: &now, Questions: []JevQuestion{attention}},
		JevBattery{Name: "landing", Items: 80, Labeled: 80, ScoredAt: &now, Questions: []JevQuestion{landing}},
		JevBattery{Name: "finding", Items: 140, Labeled: 140, ScoredAt: &now, Questions: []JevQuestion{finding, watching}},
		JevBattery{Name: "progress", Items: 50, Labeled: 50, ScoredAt: &now, Questions: []JevQuestion{progress}},
	)
	s := BuildJevSummary(sc, nil, nil)
	if s == nil {
		t.Fatal("a scorecard built no summary")
	}
	if len(s.Earning) != 4 {
		t.Fatalf("Earning = %+v, want the four earning questions", s.Earning)
	}

	got := s.Earning[0]
	if got.Battery != "attention" || got.ID != "needs_human" || got.Labeled != 60 || got.Positives != 20 {
		t.Errorf("attention row = %+v", got)
	}
	if !jevSummaryClose(jevSummaryFloat(t, "attention base rate", got.BaseRate), 0.25) ||
		!jevSummaryClose(jevSummaryFloat(t, "attention precision at the cut", got.PrecisionAtCut), 0.6) ||
		!jevSummaryClose(jevSummaryFloat(t, "attention lift", got.Lift), 0.35) {
		t.Errorf("attention figures = base %v, precision %v, lift %v; want 0.25, 0.6, 0.35", got.BaseRate, got.PrecisionAtCut, got.Lift)
	}
	if got.UsedBy != "gt attention and dashboard queue order" {
		t.Errorf("attention used by %q", got.UsedBy)
	}
	if got.NoLift != "" {
		t.Errorf("a noul question says it has no lift: %q", got.NoLift)
	}

	if got := s.Earning[1]; got.UsedBy != "risk-path order" {
		t.Errorf("landing risk used by %q, want risk-path order", got.UsedBy)
	}
	if got := s.Earning[2]; got.UsedBy != "none" {
		t.Errorf("finding used by %q, want none", got.UsedBy)
	}

	score := s.Earning[3]
	if score.Lift != nil || score.PrecisionAtCut != nil {
		t.Errorf("a score question has a lift %v or precision %v, want neither", score.Lift, score.PrecisionAtCut)
	}
	if !strings.Contains(score.NoLift, "score question") {
		t.Errorf("NoLift = %q, want the score question's reason", score.NoLift)
	}
}

func TestJevSummaryGroupsNotYetByCause(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	rework := JevBattery{Name: "rework", Items: 40, Labeled: 30, ScoredAt: &now, Questions: []JevQuestion{
		jevSummaryQuestion("no_rule", "not-yet", 0, 0, 0),
		jevSummaryQuestion("all_no", "not-yet", 12, 0, 12),
		jevSummaryQuestion("all_yes", "not-yet", 12, 12, 0),
		jevSummaryQuestion("both", "not-yet", 12, 5, 7),
	}}
	landing := JevBattery{Name: "landing", Items: 9, Labeled: 0, ScoredAt: &now, Questions: []JevQuestion{
		jevSummaryQuestion("risk", "not-yet", 0, 0, 0),
	}}
	s := BuildJevSummary(jevSummaryScored(now, rework, landing), nil, nil)
	if s == nil {
		t.Fatal("a scorecard built no summary")
	}

	if want := (JevCause{Count: 2, Names: []string{"rework.no_rule", "landing.risk"}}); !reflect.DeepEqual(s.Stuck.NoLabelRule, want) {
		t.Errorf("no label rule = %+v, want %+v", s.Stuck.NoLabelRule, want)
	}
	if want := (JevCause{Count: 1, Names: []string{"rework.all_yes"}}); !reflect.DeepEqual(s.Stuck.OnlyPositive, want) {
		t.Errorf("only positive labels = %+v, want %+v", s.Stuck.OnlyPositive, want)
	}
	if want := (JevCause{Count: 1, Names: []string{"rework.all_no"}}); !reflect.DeepEqual(s.Stuck.ZeroPositives, want) {
		t.Errorf("zero positives = %+v, want %+v", s.Stuck.ZeroPositives, want)
	}
	if want := []JevNotYet{{Name: "rework.both", Labeled: 12, Positives: 5, Negatives: 7}}; !reflect.DeepEqual(s.Watching, want) {
		t.Errorf("watching = %+v, want %+v", s.Watching, want)
	}
}

func TestJevSummaryNamesReusedBlocksAndDoesNotCallThemOld(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	sc := jevSummaryScored(now,
		JevBattery{Name: "finding", Items: 5, ScoredAt: ago(2 * time.Hour)},
		JevBattery{Name: "notes", Items: 5, ScoredAt: ago(30 * time.Minute)},
		JevBattery{Name: "targets", Items: 5, ScoredAt: ago(time.Hour)},
		JevBattery{Name: "rules", Items: 5, ScoredAt: ago(61 * time.Minute)},
		JevBattery{Name: "landing", Items: 5, ScoredAt: ago(3 * time.Hour), Failed: true, Kept: true},
		JevBattery{Name: "drift", Items: 5, Failed: true},
	)
	s := BuildJevSummary(sc, nil, nil)
	if s == nil {
		t.Fatal("a scorecard built no summary")
	}

	if s.Refreshed == nil || !s.Refreshed.Equal(now) {
		t.Errorf("Refreshed = %v, want the cache's scored_at %v", s.Refreshed, now)
	}
	want := []JevReused{
		{Battery: "finding", ScoredAt: now.Add(-2 * time.Hour), Reason: "log unchanged"},
		{Battery: "rules", ScoredAt: now.Add(-61 * time.Minute), Reason: "log unchanged"},
		{Battery: "landing", ScoredAt: now.Add(-3 * time.Hour), Reason: "log unchanged"},
	}
	if !reflect.DeepEqual(s.Reused, want) {
		t.Errorf("Reused = %+v, want %+v: a block within the hour is this refresh's, and a failed battery with no block is not reused", s.Reused, want)
	}

	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{`"old"`, `"stale"`, `"age"`} {
		if strings.Contains(string(out), word) {
			t.Errorf("the summary calls a reused block %s: %s", word, out)
		}
	}
}

func TestJevSummarySpendAgainstTheCapOrAbsent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	sc := jevSummaryScored(now, JevBattery{Name: "finding", Items: 1, ScoredAt: &now})
	spend := &JevSpend{Calls: 14, USD: 1.75, Window: "24h", Periods: &JevPeriods{TodayUSD: 0.8}}

	s := BuildJevSummary(sc, spend, ptrFloat(2.5))
	if got := s.Spend; got.Calls == nil || *got.Calls != 14 || jevSummaryFloat(t, "window dollars", got.WindowUSD) != 1.75 ||
		got.Window != "24h" || jevSummaryFloat(t, "today's dollars", got.TodayUSD) != 0.8 || jevSummaryFloat(t, "the cap", got.CapUSD) != 2.5 {
		t.Errorf("spend = %+v, want the report's figures beside the cap", got)
	}

	// A spend row with no periods leaves today's dollars absent, not zero, and a
	// cap that is not above zero is no cap.
	s = BuildJevSummary(sc, &JevSpend{Calls: 1, USD: 0.5, Window: "24h"}, ptrFloat(0))
	if s.Spend.TodayUSD != nil {
		t.Errorf("today's dollars = %v, want absent when the report prints no periods", *s.Spend.TodayUSD)
	}
	if s.Spend.CapUSD != nil {
		t.Errorf("a zero cap is drawn as %v, want absent", *s.Spend.CapUSD)
	}

	// A negative cap is configured but has no usable figure, so the page must
	// say the cap is unknown rather than unset.
	s = BuildJevSummary(sc, spend, ptrFloat(-3))
	if s.Spend.CapUSD != nil || !s.Spend.CapUnreadable {
		t.Errorf("a negative cap = %+v, want no figure and the cap marked unreadable", s.Spend)
	}

	// With no spend row and no cap, every figure is absent from the JSON too.
	s = BuildJevSummary(sc, nil, nil)
	out, err := json.Marshal(s.Spend)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "{}" {
		t.Errorf("an unread spend row encodes as %s, want no figures", out)
	}
}

func TestJevSummaryListsOffBatteriesOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	off := JevQuestion{ID: "not_yet_asked", Verdict: "not-yet", Labeled: 0}
	earning := jevSummaryQuestion("would_earn", "earning", 80, 40, 40)
	earning.BaseRate, earning.PrecisionAtBest = ptrFloat(0.5), ptrFloat(0.8)
	sc := jevSummaryScored(now,
		JevBattery{Name: "dedupe", Items: 0, Questions: []JevQuestion{off}},
		JevBattery{Name: "testsel", Items: 39858, ScoredAt: &now, Questions: []JevQuestion{earning}},
		JevBattery{Name: "testsel", Items: 39858, ScoredAt: &now, Questions: []JevQuestion{earning}},
		JevBattery{Name: "drift", Items: 0, ScoredAt: &now, Questions: []JevQuestion{off}},
		JevBattery{Name: "notes", Items: 0, ScoredAt: &now, Questions: []JevQuestion{off}},
		JevBattery{Name: "landing", Failed: true},
	)
	s := BuildJevSummary(sc, nil, nil)
	if s == nil {
		t.Fatal("a scorecard built no summary")
	}

	if want := []string{"dedupe", "testsel", "drift"}; !reflect.DeepEqual(s.Off, want) {
		t.Errorf("Off = %q, want %q: each off battery once, and an empty patrol battery is not off", s.Off, want)
	}
	for _, e := range s.Earning {
		if e.Battery == "testsel" || e.Battery == "dedupe" {
			t.Errorf("an off battery's question is in Earning: %+v", e)
		}
	}
	if len(s.Watching) != 0 || s.Stuck.NoLabelRule.Count != 1 || s.Stuck.NoLabelRule.Names[0] != "notes.not_yet_asked" {
		t.Errorf("groups hold an off battery's question: watching %+v, stuck %+v", s.Watching, s.Stuck)
	}
	if len(s.Retire) != 0 {
		t.Errorf("Retire = %+v, want none", s.Retire)
	}
}

// BuildJevSummary is nil without a scorecard, so the page has no summary to draw
// before the first read.
func TestJevSummaryIsNilWithoutAScorecard(t *testing.T) {
	t.Parallel()

	if s := BuildJevSummary(nil, &JevSpend{Calls: 1, USD: 1}, ptrFloat(1)); s != nil {
		t.Errorf("no scorecard built %+v, want nil", *s)
	}
}

func ptrFloat(v float64) *float64 { return &v }

// The hub builds the summary on each Jev read, from the last good scorecard and
// spend row, so a failed scorecard read keeps the summary the page last drew.
func TestHubBuildsTheJevSummaryOnEachRead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ok := true
	h := NewHub(Config{
		JevSpend: func() *JevSpend { return &JevSpend{Calls: 3, USD: 0.5, Window: "24h"} },
		JevScorecard: func() *JevScorecard {
			if !ok {
				return nil
			}
			return jevSummaryScored(now, JevBattery{Name: "finding", Items: 2, Labeled: 1, ScoredAt: &now})
		},
		JevCap: func() (*float64, error) { return ptrFloat(2), nil },
	})
	if st := h.State(); st.JevSummary != nil {
		t.Fatalf("a summary before any read: %+v", st.JevSummary)
	}
	h.pollJevScorecard()
	h.pollJevSpend()

	st := h.State()
	if st.JevSummary == nil {
		t.Fatal("a read left no summary")
	}
	if st.JevSummary.Spend.CapUSD == nil || *st.JevSummary.Spend.CapUSD != 2 ||
		st.JevSummary.Spend.Calls == nil || *st.JevSummary.Spend.Calls != 3 {
		t.Errorf("summary spend = %+v, want the spend row beside the cap", st.JevSummary.Spend)
	}

	ok = false
	h.pollJevScorecard()
	if got := h.State().JevSummary; got == nil || got.Refreshed == nil || !got.Refreshed.Equal(now) {
		t.Errorf("a failed scorecard read left summary %+v, want the last good refresh kept", got)
	}
}

// A cap the config could not be read for is marked unreadable on the summary
// and carries no figure, so the page cannot take it for a town with no cap.
func TestHubMarksAnUnreadableJevCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		JevSpend: func() *JevSpend { return &JevSpend{Calls: 3, USD: 0.5, Window: "24h"} },
		JevScorecard: func() *JevScorecard {
			return jevSummaryScored(now, JevBattery{Name: "finding", Items: 2, Labeled: 1, ScoredAt: &now})
		},
		JevCap: func() (*float64, error) { return nil, errors.New("reading daemon.json: permission denied") },
	})
	h.pollJevScorecard()
	h.pollJevSpend()

	got := h.State().JevSummary
	if got == nil {
		t.Fatal("a read left no summary")
	}
	if !got.Spend.CapUnreadable || got.Spend.CapUSD != nil {
		t.Errorf("spend = %+v, want the cap marked unreadable with no figure", got.Spend)
	}
	if got.Spend.Calls == nil || *got.Spend.Calls != 3 {
		t.Errorf("an unreadable cap dropped the spend figures: %+v", got.Spend)
	}
}
