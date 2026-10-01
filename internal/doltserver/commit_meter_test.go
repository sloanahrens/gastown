package doltserver

import (
	"reflect"
	"testing"
)

// OverCommitBudget keeps readings strictly above the limit, busiest first,
// and never counts a failed reading.
func TestOverCommitBudget(t *testing.T) {
	t.Parallel()

	counts := []DBCommits{
		{Database: "be", Commits: 17},
		{Database: "gt", Commits: 3048},
		{Database: "hm", Commits: 500},
		{Database: "hq", Commits: 2041},
		{Database: "om", Err: "timeout"},
	}
	got := OverCommitBudget(counts, 500)
	want := []DBCommits{{Database: "gt", Commits: 3048}, {Database: "hq", Commits: 2041}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OverCommitBudget = %+v, want %+v", got, want)
	}
	if got := OverCommitBudget(counts, 5000); got != nil {
		t.Errorf("OverCommitBudget above every count = %+v, want nil", got)
	}
}

func TestFormatCommitCounts(t *testing.T) {
	t.Parallel()

	got := FormatCommitCounts([]DBCommits{{Database: "gt", Commits: 3048}, {Database: "om", Err: "timeout"}})
	if want := "gt=3048 om=?"; got != want {
		t.Errorf("FormatCommitCounts = %q, want %q", got, want)
	}
}
