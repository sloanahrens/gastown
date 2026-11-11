package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// jevHistoryNow is the refresh instant the history tests share.
var jevHistoryNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// jevQuestionScorer stands in for the adapters with one question per battery,
// labeled as the test sets it, so a refresh's history row has counts to record.
func jevQuestionScorer(labeled int) jevBatteryScorer {
	return func(_ context.Context, name, _ string, _ time.Time) (scorecard.Report, error) {
		return scorecard.Report{Battery: name, Labeled: labeled, Questions: []scorecard.QuestionReport{
			{ID: "severity", Kind: scorecard.KindNoul, Labeled: labeled, Positives: 2, Verdict: scorecard.VerdictNotYet},
		}}, nil
	}
}

// Three refreshes that leave a question's labeled count unchanged make the
// history view say so, once the third row shares the count.
func TestJevScorecardHistoryViewReportsNoGrowthAfterThreeEqualRows(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := scorecard.HistoryPath(town)
	for i := 0; i < 3; i++ {
		at := jevHistoryNow.Add(time.Duration(i-2) * time.Hour)
		row := scorecard.HistoryRow{At: at, Since: "7d", Batteries: []scorecard.HistoryBattery{{
			Name:      "logtriage",
			Questions: []scorecard.HistoryQuestion{{ID: "severity", Labeled: 14, Positives: 2, Verdict: scorecard.VerdictNotYet}},
		}}}
		(scorecard.Cache{}).AppendHistory(path, row, at, nil)
	}
	var out bytes.Buffer

	err := jevScorecardRun(context.Background(), &out, town,
		jevScorecardOptions{History: true, Battery: "logtriage", Question: "severity"},
		jevHistoryNow, nil, scorecard.Cache{})
	if err != nil {
		t.Fatalf("viewing the history: %v", err)
	}
	if !strings.Contains(out.String(), "no growth in 3 refreshes") {
		t.Errorf("view lacks the no-growth line:\n%s", out.String())
	}
}

// A refresh appends one row with the batteries it scored. A battery reused from
// the cache has no entry in that row.
func TestJevScorecardRefreshHistoryOmitsReusedBattery(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeBatteryLog(t, town, "finding", "one\ntwo\n")
	prev := scorecard.CacheDoc{
		ScoredAt: jevHistoryNow.Add(-24 * time.Hour),
		Since:    "7d",
		Batteries: []scorecard.BatteryBlock{{
			Name: "finding", ScoredAt: jevHistoryNow.Add(-24 * time.Hour), LogLines: 2, LogBytes: 8,
			Report: &scorecard.Report{Battery: "finding", Labeled: 1},
		}},
	}
	if err := (scorecard.Cache{}).Write(scorecard.CachePath(town), prev); err != nil {
		t.Fatal(err)
	}

	if err := jevScorecardRun(context.Background(), &bytes.Buffer{}, town,
		jevScorecardOptions{Refresh: true}, jevHistoryNow, jevQuestionScorer(9), scorecard.Cache{}); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	rows, err := (scorecard.Cache{}).ReadHistory(scorecard.HistoryPath(town))
	if err != nil {
		t.Fatalf("reading the history: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history has %d rows after one refresh, want 1", len(rows))
	}
	var names []string
	for _, b := range rows[0].Batteries {
		if b.Name == "finding" {
			t.Errorf("row names the reused battery finding: %+v", rows[0].Batteries)
		}
		names = append(names, b.Name)
	}
	if len(names) != len(scorecard.Registered())-1 {
		t.Errorf("row names %d batteries, want every scored battery but finding", len(names))
	}
}

// A history write that fails is logged, and the refresh still writes the cache
// and exits zero.
func TestJevScorecardRefreshSurvivesAHistoryWriteFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cache := scorecard.Cache{WriteFile: func(path string, data []byte, perm os.FileMode) error {
		if strings.HasSuffix(path, "scorecard.history.jsonl") {
			return os.ErrPermission
		}
		return atomicfile.WriteFile(path, data, perm)
	}}
	var out bytes.Buffer

	if err := jevScorecardRun(context.Background(), &out, town,
		jevScorecardOptions{Refresh: true}, jevHistoryNow, jevQuestionScorer(9), cache); err != nil {
		t.Fatalf("refresh failed on a history write: %v", err)
	}
	if !strings.Contains(out.String(), "scorecard history not appended") {
		t.Errorf("output lacks the history log line:\n%s", out.String())
	}
	readJevCache(t, town)
	if _, err := os.Stat(scorecard.HistoryPath(town)); !os.IsNotExist(err) {
		t.Errorf("history file exists after its write failed: %v", err)
	}
}

// The history view with no history yet says so and exits zero.
func TestJevScorecardHistoryViewWithNoFileSaysSo(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	if err := jevScorecardRun(context.Background(), &out, t.TempDir(),
		jevScorecardOptions{History: true}, jevHistoryNow, nil, scorecard.Cache{}); err != nil {
		t.Fatalf("viewing an absent history: %v", err)
	}
	if !strings.Contains(out.String(), "no scorecard history yet") {
		t.Errorf("output = %q, want the no-history message", out.String())
	}
}

// A history file with a line that does not parse is refused by the view, not
// shown with that row missing.
func TestJevScorecardHistoryViewRefusesACorruptHistory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := scorecard.HistoryPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not a row\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	err := jevScorecardRun(context.Background(), &out, town,
		jevScorecardOptions{History: true}, jevHistoryNow, nil, scorecard.Cache{})
	if !errors.Is(err, scorecard.ErrCorruptHistory) {
		t.Errorf("err = %v, want ErrCorruptHistory", err)
	}
	if out.Len() != 0 {
		t.Errorf("view printed %q for a corrupt history, want nothing", out.String())
	}
}

// The history view reads the file only, so a flag that shapes a scoring run is
// refused, and --question means nothing outside the view.
func TestJevScorecardHistoryRefusesScoringFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		o    jevScorecardOptions
	}{
		{"refresh", jevScorecardOptions{History: true, Refresh: true}},
		{"json", jevScorecardOptions{History: true, JSON: true}},
		{"lines", jevScorecardOptions{History: true, Lines: true}},
		{"since", jevScorecardOptions{History: true, Since: "7d"}},
		{"question without history", jevScorecardOptions{Battery: "finding", Question: "fix_worthy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := jevScorecardRun(context.Background(), &bytes.Buffer{}, t.TempDir(),
				tc.o, jevHistoryNow, nil, scorecard.Cache{})
			if err == nil {
				t.Error("run succeeded, want a refusal")
			}
		})
	}
}
