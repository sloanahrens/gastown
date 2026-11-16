package cmd

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// usageRowFor is one usage_records row: the run's record, built at its end.
func usageRowFor(bead, kind string, end time.Time, wallS int64) beads.UsageRecordRow {
	return beads.UsageRecordRow{
		BuiltAt: end,
		Record: dashboard.UsageRecord{
			Bead:     bead,
			Kind:     kind,
			Class:    "polecat",
			Rig:      "gastown",
			Complete: true,
			Start:    end.Add(-time.Minute),
			End:      end,
			WallS:    wallS,
		},
	}
}

// usageRowsFixed is a row store that answers every read with rows.
func usageRowsFixed(rows ...beads.UsageRecordRow) usageRowsRead {
	return func(time.Time, int) ([]beads.UsageRecordRow, error) {
		return rows, nil
	}
}

// findUsageRecords returns the records of the set for bead and kind, in order.
func findUsageRecords(recs []dashboard.UsageRecord, bead, kind string) []dashboard.UsageRecord {
	var out []dashboard.UsageRecord
	for _, rec := range recs {
		if rec.Bead == bead && rec.Kind == kind {
			out = append(out, rec)
		}
	}
	return out
}

// A row and the file of the same bead and kind are one record: the row wins, and
// the file's older figure is not counted beside it.
func TestUsageRowsWinOverTheFileOfTheSameBeadAndKind(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-a.work.json",
		usageRecordBody("gt-a", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)

	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowFor("gt-a", dashboard.UsageKindWork, usageReaderNow.Add(-time.Hour), 900)))

	got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-a", dashboard.UsageKindWork)
	if len(got) != 1 || got[0].WallS != 900 {
		t.Fatalf("gt-a work records = %+v, want the one row with wall 900", got)
	}
}

// A file is held back only by a row of its own bead and kind: a work row for a
// bead does not hide the bead's om file.
func TestUsageRowsDoNotHideAFileOfAnotherKind(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-b.om.json",
		usageRecordBody("gt-b", "om", "om-review", usageReaderNow.Add(-2*time.Hour), 600), 2*time.Hour)

	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowFor("gt-b", dashboard.UsageKindWork, usageReaderNow.Add(-time.Hour), 200)))

	recs := r.monthRecords(usageReaderNow)
	if got := findUsageRecords(recs, "gt-b", dashboard.UsageKindOM); len(got) != 1 || got[0].WallS != 600 {
		t.Fatalf("gt-b om records = %+v, want the file's record with wall 600", got)
	}
	if got := findUsageRecords(recs, "gt-b", dashboard.UsageKindWork); len(got) != 1 || got[0].WallS != 200 {
		t.Fatalf("gt-b work records = %+v, want the row with wall 200", got)
	}
}

// A run filed only as a row still reads once the records directory is gone, since
// a later bead removes the file path and the row outlives it.
func TestUsageRowsCountARecordFiledOnlyAsARow(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	if err := os.RemoveAll(records); err != nil {
		t.Fatal(err)
	}

	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowFor("gt-c", dashboard.UsageKindWork, usageReaderNow.Add(-time.Hour), 450)))

	u := r.read(usageReaderNow)
	if u.Unavailable {
		t.Fatalf("a row-only reading came back unavailable: %+v", u)
	}
	if got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-c", dashboard.UsageKindWork); len(got) != 1 {
		t.Fatalf("gt-c records = %+v, want the one row", got)
	}
}

// A row read that fails is an unavailable panel and an absent plan month, never
// the files alone: a row the files lack would be dropped without saying so. The
// panel names the failure in fixed words, not in the store's own error.
func TestUsageRowsReadFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-a.work.json",
		usageRecordBody("gt-a", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)

	failing := func(time.Time, int) ([]beads.UsageRecordRow, error) {
		return nil, errors.New("sample list: bd is down")
	}
	r := newDashUsageReader(root).withRows(failing)

	if u := r.read(usageReaderNow); !u.Unavailable || u.Reason != usageRowsReadReason {
		t.Errorf("read = %+v, want unavailable with the rows-read reason", u)
	}
	if month := r.monthRecords(usageReaderNow); month != nil {
		t.Errorf("month = %+v, want no record set", month)
	}
}

// A row read that fills its cap is refused, never folded: the rows that fell off
// the cap are the window's oldest.
func TestUsageRowsCapFilledIsRefused(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	full := make([]beads.UsageRecordRow, usageRowsLimit)
	for i := range full {
		full[i] = usageRowFor("gt-full", dashboard.UsageKindWork, usageReaderNow.Add(-time.Hour), 1)
	}
	r := newDashUsageReader(root).withRows(usageRowsFixed(full...))

	if u := r.read(usageReaderNow); !u.Unavailable || u.Reason != usageRowsCapReason {
		t.Errorf("read over a filled cap = %+v, want unavailable with the cap reason", u)
	}
	if month := r.monthRecords(usageReaderNow); month != nil {
		t.Errorf("month over a filled cap = %d records, want none", len(month))
	}
}

// usageRowWithSessions is a row of bead and kind that counts sessions.
func usageRowWithSessions(bead, kind string, end time.Time, wallS int64, sessions ...string) beads.UsageRecordRow {
	row := usageRowFor(bead, kind, end, wallS)
	row.Sessions = sessions
	return row
}

// A row written again for the same sessions is the same run, so it counts once:
// the usage record writer drops a repeated write the same way.
func TestUsageRowsRepeatedWriteCountsOnce(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	end := usageReaderNow.Add(-time.Hour)
	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowWithSessions("gt-d", dashboard.UsageKindWork, end, 700, "s1", "s2"),
		usageRowWithSessions("gt-d", dashboard.UsageKindWork, end, 700, "s1", "s2"),
	))

	got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-d", dashboard.UsageKindWork)
	if len(got) != 1 || got[0].WallS != 700 {
		t.Fatalf("gt-d work records = %+v, want the one run with wall 700", got)
	}
}

// Rows of one bead and kind that count different sessions are different runs, so
// both count.
func TestUsageRowsDisjointSessionsOfOneBeadBothCount(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowWithSessions("gt-e", dashboard.UsageKindWork, usageReaderNow.Add(-time.Hour), 400, "s1"),
		usageRowWithSessions("gt-e", dashboard.UsageKindWork, usageReaderNow.Add(-2*time.Hour), 250, "s2"),
	))

	got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-e", dashboard.UsageKindWork)
	if len(got) != 2 {
		t.Fatalf("gt-e work records = %+v, want both runs", got)
	}
}

// Rows of one bead and kind that share a session but differ cannot be split into
// their totals, so the panel is refused rather than counting the session twice.
func TestUsageRowsSharedSessionWithDifferentTotalsIsRefused(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	end := usageReaderNow.Add(-time.Hour)
	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowWithSessions("gt-f", dashboard.UsageKindWork, end, 700, "s1", "s2"),
		usageRowWithSessions("gt-f", dashboard.UsageKindWork, end, 300, "s2"),
	))

	if u := r.read(usageReaderNow); !u.Unavailable || u.Reason != usageRowsConflictReason {
		t.Errorf("read = %+v, want unavailable with the conflict reason", u)
	}
	if month := r.monthRecords(usageReaderNow); month != nil {
		t.Errorf("month = %d records, want none", len(month))
	}
}

// With no records directory and no row store wired, the panel names the absence
// as it did before the rows: the directory is the only record source.
func TestUsageRowsUnwiredMissingDirectoryKeepsItsReason(t *testing.T) {
	t.Parallel()

	u := newDashUsageReader(t.TempDir()).read(usageReaderNow)
	if !u.Unavailable || u.Reason != dashboard.NotInClusterReason {
		t.Errorf("read = %+v, want unavailable with the not-in-cluster reason", u)
	}
}
