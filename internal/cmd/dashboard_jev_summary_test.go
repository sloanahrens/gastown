package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// The panel's summary reads the cache's top-level scored_at and each question's
// kind, split of labels and precision at the cut, so the summary can say what
// earns and what is waiting on labels.
func TestDashboardJevScorecardCarriesTheSummaryFields(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	scored := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	base, precisionAtBest := 0.25, 0.6
	doc := scorecard.CacheDoc{ScoredAt: scored, Since: "7d", Batteries: []scorecard.BatteryBlock{
		{Name: "attention", ScoredAt: scored.Add(-2 * time.Hour), Report: &scorecard.Report{
			Battery: "attention", Items: 100, Labeled: 60,
			Questions: []scorecard.QuestionReport{
				{ID: "needs_human", Kind: "noul", Verdict: "earning", Labeled: 60, Positives: 20, Negatives: 40,
					BaseRate: &base, PrecisionAtBest: &precisionAtBest},
			},
		}},
	}}
	writeJevCache(t, town, doc)

	sc := dashboardJevScorecard(town)()
	if sc == nil {
		t.Fatal("the cache read as a failure")
	}
	if sc.ScoredAt == nil || !sc.ScoredAt.Equal(scored) {
		t.Errorf("ScoredAt = %v, want the cache's scored_at %v", sc.ScoredAt, scored)
	}
	if len(sc.Batteries) != 1 || len(sc.Batteries[0].Questions) != 1 {
		t.Fatalf("scorecard = %+v", *sc)
	}
	q := sc.Batteries[0].Questions[0]
	if q.Kind != "noul" || q.Positives != 20 || q.Negatives != 40 ||
		q.PrecisionAtBest == nil || *q.PrecisionAtBest != precisionAtBest {
		t.Errorf("question = %+v, want its kind, label split and precision at the cut", q)
	}
}

// The cap is the dispatch battery's max_usd_per_day, the town-wide cap the
// config names. A town with no cap configured reads no cap and no error. A
// config that cannot be read is an error, never a quiet no cap.
func TestDashboardJevCapReadsTheDispatchCap(t *testing.T) {
	t.Parallel()

	const noDispatch = `{"type":"daemon-patrol-config","version":1,"patrols":{}}`
	cases := []struct {
		name    string
		body    string // "" writes no patrol config at all
		want    *float64
		wantErr bool
	}{
		{"no patrol config", "", nil, false},
		{"no dispatch battery", noDispatch, nil, false},
		{"a configured cap", `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_dispatch":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":42}}}`, ptrFloat(2.5), false},
		{"a dispatch battery with no cap takes the default", `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_dispatch":{"enabled":true,"usd_per_billion_tokens":42}}}`, ptrFloat(dispatch.DefaultMaxUSDPerDay), false},
		{"an unreadable config", `not json`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			town := t.TempDir()
			if tc.body != "" {
				if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(town, "mayor", "daemon.json"), []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := dashboardJevCap(town)()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want an error: %v", err, tc.wantErr)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("cap = %v, want none", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("cap = %v, want %v", got, *tc.want)
			}
		})
	}
}

func ptrFloat(v float64) *float64 { return &v }
