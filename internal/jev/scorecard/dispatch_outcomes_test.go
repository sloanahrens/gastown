package scorecard

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
)

// landedStore holds one bead closed by a landing, so its placement is labeled
// and its outcomes reach the labeling.
func landedStore(bead string) dispatchBeadStore {
	return dispatchBeadStore{
		bead: {ID: bead, Status: "closed", CloseReason: "Landed on main\nlanded_commit: 1111"},
	}
}

// The dispatch battery writes its two outcomes, in this order.
func TestDispatchDeclaresTheTwoOutcomeColumns(t *testing.T) {
	t.Parallel()
	got := dispatchAdapterFor(t.TempDir(), dispatchBeadStore{}).Outcomes()
	want := []Outcome{OutcomeRanOverTwenty, OutcomeNeededSecondRound}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Outcomes() = %v, want %v", got, want)
	}
}

// Read hands the adapter's declared outcomes to the reading, so the dataset can
// write its columns.
func TestDispatchReadCarriesTheDeclaredOutcomes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	writeDispatchFixture(t, town, []dispatch.Line{
		{Bead: "gt-o", AttachedAt: at, Answers: dispatchAnswers(0.9)},
	}, nil, nil, nil)
	read, err := Read(context.Background(), dispatchAdapterFor(town, landedStore("gt-o")), time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(read.Outcomes) != 2 || read.Outcomes[0] != OutcomeRanOverTwenty || read.Outcomes[1] != OutcomeNeededSecondRound {
		t.Errorf("Reading.Outcomes = %v, want the two dispatch outcomes", read.Outcomes)
	}
}

// ran_over_twenty is true only past twenty minutes of polecat time, and reads
// as unknown when the landing carries no weighted time for the bead.
func TestDispatchRanOverTwentyOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rec  *landings.Record
		want bool
		set  bool
	}{
		{name: "a polecat landing 21 minutes long is over twenty", rec: &landings.Record{Bead: "gt-o", Agent: "sbx-haiku", PolecatSecs: 21 * 60}, want: true, set: true},
		{name: "a polecat landing exactly twenty minutes long is not", rec: &landings.Record{Bead: "gt-o", Agent: "sbx-haiku", PolecatSecs: 20 * 60}, want: false, set: true},
		{name: "a polecat landing 19 minutes long is not", rec: &landings.Record{Bead: "gt-o", Agent: "sbx-haiku", PolecatSecs: 19 * 60}, set: true},
		{name: "a bead with no landing record is unknown", rec: nil},
		{name: "a crew landing is unknown", rec: &landings.Record{Bead: "gt-o", PolecatSecs: 30 * 60}},
		{name: "a polecat landing with a zero time is unknown", rec: &landings.Record{Bead: "gt-o", Agent: "sbx-haiku"}},
		{name: "a polecat landing with a lower bound is unknown", rec: &landings.Record{Bead: "gt-o", Agent: "sbx-haiku", PolecatSecs: 30 * 60, PolecatLowerBound: true}},
		{name: "a polecat landing with an upper bound is unknown", rec: &landings.Record{Bead: "gt-o", Agent: "sbx-haiku", PolecatSecs: 30 * 60, PolecatUpperBound: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.rec != nil {
				writeLandingsFixture(t, town, []landings.Record{*tc.rec})
			}
			got, err := dispatchAdapterFor(town, landedStore("gt-o")).Label(context.Background(), Item{ID: "gt-o"})
			if err != nil {
				t.Fatalf("Label: %v", err)
			}
			v, ok := got.Outcomes[OutcomeRanOverTwenty]
			if ok != tc.set {
				t.Fatalf("ran_over_twenty set = %v, want %v (value %v)", ok, tc.set, v)
			}
			if ok && v != tc.want {
				t.Errorf("ran_over_twenty = %v, want %v", v, tc.want)
			}
		})
	}
}

// needed_second_round is true when the bead had a verdict from attempt 2 or
// later, false when its only verdict was the first attempt, and unknown when it
// has no verdict at all.
func TestDispatchNeededSecondRoundOutcome(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	first := land.ReviewRecord{BeadID: "gt-o", Attempt: 1, Verdict: "approve", At: at}
	second := land.ReviewRecord{BeadID: "gt-o", Attempt: 2, Verdict: "approve", At: at.Add(time.Hour)}
	cases := []struct {
		name    string
		reviews []land.ReviewRecord
		want    bool
		set     bool
	}{
		{name: "no verdict is unknown", reviews: nil},
		{name: "only a first-attempt verdict is false", reviews: []land.ReviewRecord{first}, want: false, set: true},
		{name: "a second-attempt verdict alone is true", reviews: []land.ReviewRecord{second}, want: true, set: true},
		{name: "a first and a second verdict is true", reviews: []land.ReviewRecord{first, second}, want: true, set: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeDispatchFixture(t, town, nil, nil, tc.reviews, nil)
			got, err := dispatchAdapterFor(town, landedStore("gt-o")).Label(context.Background(), Item{ID: "gt-o"})
			if err != nil {
				t.Fatalf("Label: %v", err)
			}
			v, ok := got.Outcomes[OutcomeNeededSecondRound]
			if ok != tc.set {
				t.Fatalf("needed_second_round set = %v, want %v (value %v)", ok, tc.set, v)
			}
			if ok && v != tc.want {
				t.Errorf("needed_second_round = %v, want %v", v, tc.want)
			}
		})
	}
}
