package dataset

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// outcomeReading is fixtureReading with the dispatch battery's two outcomes
// declared. The first item carries both outcomes, the second only a
// ran_over_twenty, so needed_second_round is unreadable there, and the third is
// unlabeled and has no row.
func outcomeReading() scorecard.Reading {
	read := fixtureReading()
	read.Outcomes = []scorecard.Outcome{"ran_over_twenty", "needed_second_round"}
	read.Labelings[0].Outcomes = map[scorecard.Outcome]bool{"ran_over_twenty": true, "needed_second_round": false}
	read.Labelings[1].Outcomes = map[scorecard.Outcome]bool{"ran_over_twenty": false}
	return read
}

// The dispatch battery's dataset carries its two outcomes after label_source,
// true, false and empty cells alike, and the dict names both with their source
// and the twenty-minute threshold.
func TestDispatchDatasetCarriesTheTwoOutcomeColumns(t *testing.T) {
	t.Parallel()
	table, err := Build(outcomeReading(), "needs")
	if err != nil {
		t.Fatal(err)
	}
	want := `row_id,day,hour,dow,model,answer,answer_band,confidence,label,label_rule,label_source,ran_over_twenty,needed_second_round,ref_rig,ref_agent,ans_route
5de8462c2176,2026-10-18,10,0,m2,0.2,0.2-0.4,,false,fixture-labels,,false,,gastown,crew,no
2cde2b79325a,2026-10-18,9,0,m1,0.9,0.8-1.0,,true,fixture-labels,overseer,true,false,gastown,polecat,yes
`
	if got := writeCSV(t, table); got != want {
		t.Errorf("CSV = %q, want %q", got, want)
	}
	dict := string(table.Dict("dispatch", "needs"))
	// The threshold is the scorecard's own value, so a second copy of the
	// number in the dict would fail here once OverTwentySecs changed.
	threshold := fmt.Sprintf("over %d minutes (%d seconds)", scorecard.OverTwentySecs/60, scorecard.OverTwentySecs)
	for _, want := range []string{
		"| ran_over_twenty | boolean |",
		"| needed_second_round | boolean |",
		threshold,
		"attempt 2 or later",
	} {
		if !strings.Contains(dict, want) {
			t.Errorf("dict does not hold %q:\n%s", want, dict)
		}
	}
	// The dict states the threshold once, not again in the same sentence.
	if got := strings.Count(dict, "minutes"); got != 1 {
		t.Errorf("dict states a minutes threshold %d times, want once:\n%s", got, dict)
	}
}

// A battery that declares no outcomes writes the bytes it wrote before, even
// when a labeling carries an outcome value: the declaration decides the columns.
func TestOtherBatteriesDatasetsAreUnchanged(t *testing.T) {
	t.Parallel()
	read := fixtureReading()
	read.Labelings[0].Outcomes = map[scorecard.Outcome]bool{"ran_over_twenty": true}
	table, err := Build(read, "needs")
	if err != nil {
		t.Fatal(err)
	}
	want := `row_id,day,hour,dow,model,answer,answer_band,confidence,label,label_rule,label_source,ref_rig,ref_agent,ans_route
5de8462c2176,2026-10-18,10,0,m2,0.2,0.2-0.4,,false,fixture-labels,,gastown,crew,no
2cde2b79325a,2026-10-18,9,0,m1,0.9,0.8-1.0,,true,fixture-labels,overseer,gastown,polecat,yes
`
	if got := writeCSV(t, table); got != want {
		t.Errorf("CSV = %q, want %q", got, want)
	}
}

// An outcome the item does not carry is an empty cell, never false: a bead
// with no landing record and no verdict reads both outcomes as unknown.
func TestOutcomeThatCannotBeReadIsAnEmptyCell(t *testing.T) {
	t.Parallel()
	read := outcomeReading()
	read.Labelings[0].Outcomes = map[scorecard.Outcome]bool{}
	table, err := Build(read, "needs")
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"ran_over_twenty", "needed_second_round"} {
		if got := cellOf(t, table, "2cde2b79325a", column); got != "" {
			t.Errorf("%s = %q for an item that carries no value, want an empty cell", column, got)
		}
	}
}

// cellOf returns the cell of the row whose id is rowID in column.
func cellOf(t *testing.T, table Table, rowID, column string) string {
	t.Helper()
	ci := slices.Index(table.Columns, column)
	if ci < 0 {
		t.Fatalf("no column %q in %v", column, table.Columns)
	}
	idx := slices.Index(table.Columns, colRowID)
	ri := slices.IndexFunc(table.Rows, func(r []string) bool { return r[idx] == rowID })
	if ri < 0 {
		t.Fatalf("no row %s", rowID)
	}
	return table.Rows[ri][ci]
}

// An outcome column may not take a name the export already writes: every cell is
// keyed by column name, so it would replace that column's value and write its
// header twice.
func TestBuildRefusesOutcomeNamedForReservedColumn(t *testing.T) {
	t.Parallel()
	for _, name := range []scorecard.Outcome{
		"row_id", "label", "model", "label_source",
		"ref_rig", "ref_head", "ans_route",
	} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			read := outcomeReading()
			read.Outcomes = []scorecard.Outcome{name}
			_, err := Build(read, "needs")
			if err == nil {
				t.Fatalf("Build accepted outcome column %q, want a refusal", name)
			}
			if !strings.Contains(err.Error(), string(name)) {
				t.Errorf("error %q does not name the outcome", err)
			}
		})
	}
}

// An outcome column is a name the dataset writes, so the deny-list refuses a
// name that may carry text just as it refuses a question's.
func TestDatasetDenyListStillRefusesPromptTextInOutcomeColumns(t *testing.T) {
	t.Parallel()
	for _, name := range []scorecard.Outcome{"prompt", "Passage"} {
		read := outcomeReading()
		read.Outcomes = []scorecard.Outcome{name}
		if _, err := Build(read, "needs"); err == nil {
			t.Errorf("Build accepted outcome column %q, want a refusal", name)
		}
	}
}
