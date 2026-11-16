package cmd

import (
	"context"
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
	return func(context.Context, time.Time, int) ([]beads.UsageRecordRow, error) {
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

// A row read that fails falls back to the files alone and the panel names the
// failure in fixed words, not in the store's own error: a store that is down does
// not blank the panel or the plan month.
func TestUsageRowsReadFailureFallsBackToTheFiles(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-a.work.json",
		usageRecordBody("gt-a", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)

	failing := func(context.Context, time.Time, int) ([]beads.UsageRecordRow, error) {
		return nil, errors.New("sample list: bd is down")
	}
	r := newDashUsageReader(root).withRows(failing)

	if u := r.read(usageReaderNow); u.Unavailable || u.Note != usageRowsReadReason {
		t.Errorf("read = %+v, want a live panel with the rows-read note", u)
	}
	if got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-a", dashboard.UsageKindWork); len(got) != 1 || got[0].WallS != 300 {
		t.Errorf("month records for gt-a = %+v, want the file's record with wall 300", got)
	}
}

// A row read that fills its cap is not folded, since the rows that fell off the
// cap are the window's oldest: the panel falls back to the files and says so.
func TestUsageRowsCapFilledFallsBackToTheFiles(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-a.work.json",
		usageRecordBody("gt-a", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)
	full := make([]beads.UsageRecordRow, usageRowsLimit)
	for i := range full {
		full[i] = usageRowFor("gt-full", dashboard.UsageKindWork, usageReaderNow.Add(-time.Hour), 1)
	}
	r := newDashUsageReader(root).withRows(usageRowsFixed(full...))

	if u := r.read(usageReaderNow); u.Unavailable || u.Note != usageRowsCapReason {
		t.Errorf("read over a filled cap = %+v, want a live panel with the cap note", u)
	}
	month := r.monthRecords(usageReaderNow)
	if got := findUsageRecords(month, "gt-full", dashboard.UsageKindWork); len(got) != 0 {
		t.Errorf("month holds %d truncated rows, want none", len(got))
	}
	if got := findUsageRecords(month, "gt-a", dashboard.UsageKindWork); len(got) != 1 {
		t.Errorf("month records for gt-a = %d, want the file's record", len(got))
	}
}

// The row read carries a deadline, so a bd that hangs cannot hold the pollers.
func TestUsageRowsReadCarriesADeadline(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	bounded := false
	r := newDashUsageReader(root).withRows(func(ctx context.Context, _ time.Time, _ int) ([]beads.UsageRecordRow, error) {
		_, bounded = ctx.Deadline()
		return nil, nil
	})

	r.read(usageReaderNow)
	if !bounded {
		t.Fatal("the usage_records read ran with no deadline")
	}
}

// A row store that blocks until its context ends is cut off at the bound: the
// read returns, the panel is drawn from the files with the timed-out note, and
// the plan month holds the file's record. The store waits only on the context,
// so the test needs no clock of its own.
func TestUsageRowsBlockingStoreIsCutOffAtTheBound(t *testing.T) {
	t.Parallel()

	root, records := usageTestDir(t)
	writeUsageRecord(t, records, "gt-a.work.json",
		usageRecordBody("gt-a", "work", "polecat", usageReaderNow.Add(-time.Hour), 300), time.Hour)
	blocking := func(ctx context.Context, _ time.Time, _ int) ([]beads.UsageRecordRow, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := newDashUsageReader(root).withRows(blocking)
	r.rowsTimeout = time.Millisecond

	if u := r.read(usageReaderNow); u.Unavailable || u.Note != usageRowsTimedOutReason {
		t.Errorf("read = %+v, want a live panel with the timed-out note", u)
	}
	if got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-a", dashboard.UsageKindWork); len(got) != 1 {
		t.Errorf("month records for gt-a = %d, want the file's record", len(got))
	}
}

// The hub's usage reader is built with the production row store wired.
func TestUsageRowsHubReaderHasTheProductionStoreWired(t *testing.T) {
	t.Parallel()

	r := newDashboardUsageReader(t.TempDir())
	if r.rows == nil {
		t.Fatal("the hub's usage reader has no row store: the rows are never read")
	}
	if r.rowsTimeout != usageRowsTimeout {
		t.Errorf("rowsTimeout = %v, want %v", r.rowsTimeout, usageRowsTimeout)
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

// A sessionless row written again is the same run, so it counts once: the same
// repeat the writer's file no-op makes.
func TestUsageRowsRepeatedSessionlessWriteCountsOnce(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	end := usageReaderNow.Add(-time.Hour)
	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowFor("gt-s", dashboard.UsageKindWork, end, 700),
		usageRowFor("gt-s", dashboard.UsageKindWork, end, 700),
	))

	got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-s", dashboard.UsageKindWork)
	if len(got) != 1 {
		t.Fatalf("gt-s work records = %+v, want the one run", got)
	}
}

// Sessionless rows that differ are different runs and cannot be shown to overlap,
// so both count.
func TestUsageRowsDifferentSessionlessRunsBothCount(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowFor("gt-t", dashboard.UsageKindWork, usageReaderNow.Add(-time.Hour), 400),
		usageRowFor("gt-t", dashboard.UsageKindWork, usageReaderNow.Add(-2*time.Hour), 250),
	))

	got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-t", dashboard.UsageKindWork)
	if len(got) != 2 {
		t.Fatalf("gt-t work records = %+v, want both runs", got)
	}
}

// A session id a row lists twice is still one session: a repeat of that row
// counts once and is not refused as a conflict.
func TestUsageRowsDuplicateSessionIDWithinARowIsNotAConflict(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	end := usageReaderNow.Add(-time.Hour)
	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowWithSessions("gt-u", dashboard.UsageKindWork, end, 500, "s1", "s1"),
		usageRowWithSessions("gt-u", dashboard.UsageKindWork, end, 500, "s1", "s1"),
	))

	u := r.read(usageReaderNow)
	if u.Unavailable {
		t.Fatalf("a repeated row with a duplicated session came back unavailable: %+v", u)
	}
	if got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-u", dashboard.UsageKindWork); len(got) != 1 {
		t.Fatalf("gt-u work records = %+v, want the one run", got)
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

// A run's row is rewritten as it gains sessions: the newer row holds the whole
// run, so the older row that shares a session with it is superseded and the
// session counts once. The panel stays live.
func TestUsageRowsRewrittenRunCountsItsNewestRowOnly(t *testing.T) {
	t.Parallel()

	root, _ := usageTestDir(t)
	end := usageReaderNow.Add(-time.Hour)
	r := newDashUsageReader(root).withRows(usageRowsFixed(
		usageRowWithSessions("gt-f", dashboard.UsageKindWork, end, 1000, "s1", "s2"),
		usageRowWithSessions("gt-f", dashboard.UsageKindWork, end, 700, "s1"),
	))

	if u := r.read(usageReaderNow); u.Unavailable || u.Note != "" {
		t.Errorf("read = %+v, want a live panel with no note", u)
	}
	got := findUsageRecords(r.monthRecords(usageReaderNow), "gt-f", dashboard.UsageKindWork)
	if len(got) != 1 || got[0].WallS != 1000 {
		t.Errorf("gt-f records = %+v, want the newest row with wall 1000 only", got)
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
