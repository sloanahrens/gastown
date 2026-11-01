package landworker

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/censustest"
)

// resetCensus makes the read that follows the only one the calling test
// counts, and drops its rows when the test ends. The census counts bd calls
// process-wide, so each test owns its reset rather than sharing one.
func resetCensus(t *testing.T) {
	t.Helper()
	beads.CensusSnapshotAndReset()
	t.Cleanup(func() { beads.CensusSnapshotAndReset() })
}

// censusReadThroughWrapper drives one real Worker read through RetryBeads over
// a fake bd and returns the census rows it recorded. The caller resets the
// census around the read.
func censusReadThroughWrapper(t *testing.T) []beads.CensusRow {
	t.Helper()
	store, _ := censustest.FakeBD(t.TempDir(), func([]string) ([]byte, error) {
		return []byte(`[{"id":"gt-krmal","updated_at":"2026-11-01T12:00:00Z"}]`), nil
	})
	w := &Worker{Beads: RetryBeads{Inner: store}}

	if row := w.outcomeRow(&beads.Issue{ID: "gt-krmal"}, true); row == "" {
		t.Fatal("outcomeRow read nothing through the wrapper")
	}
	return beads.CensusSnapshotAndReset()
}

// TestCensusNamesTheStepBehindTheRetryWrapper: the runtime stack through
// RetryBeads names the landing step that decided to read, not the wrapper
// (gt-krmal). Frame names spelled by hand cannot show the compiler writes the
// caller this way.
//
//testpolicy:allow parallel — reads the process-wide census counters, which another test must not share mid-run
func TestCensusNamesTheStepBehindTheRetryWrapper(t *testing.T) {
	resetCensus(t)
	want := beads.CensusRead{Verb: "show", Caller: "landworker.(*Worker).outcomeRow"}
	for _, row := range censusReadThroughWrapper(t) {
		if row.Verb == want.Verb && row.Caller == want.Caller {
			return
		}
	}
	t.Fatalf("the census named no %+v among its rows", want)
}

// TestCensusFoldsTheStepOntoItsWrapperRow: the read the real stack records is
// one the wrapper's budget row counts, so the gate still applies the rate
// measured under the wrapper name (gt-krmal).
//
//testpolicy:allow parallel — reads the process-wide census counters, which another test must not share mid-run
func TestCensusFoldsTheStepOntoItsWrapperRow(t *testing.T) {
	resetCensus(t)
	rows := censusReadThroughWrapper(t)
	if len(rows) == 0 {
		t.Fatal("the census recorded no rows")
	}
	counted := make(map[beads.CensusRead]bool)
	for _, read := range beads.CensusWrapperReads()["landworker.RetryBeads.Show"] {
		counted[read] = true
	}
	for _, row := range rows {
		read := beads.CensusRead{Verb: row.Verb, Caller: row.Caller}
		if !counted[read] {
			t.Errorf("census row %+v counts against no retry-wrapper budget row", read)
		}
	}
}
