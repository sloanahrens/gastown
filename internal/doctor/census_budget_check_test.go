package doctor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// censusLogStart is the first record the fixture logs: one a minute after it,
// through censusFixtureEnd, so a 60-record fixture spans an hour of stamps.
var censusLogStart = time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local)

// censusFixtureEnd is the stamp of the last record writeCensusLog writes.
var censusFixtureEnd = censusLogStart.Add(59 * time.Minute)

// censusCheck returns the check with its clock half a minute past the
// fixture's newest record, so the fixtures stay fresh however long after
// 2026-10-30 this test runs.
func censusCheck() *CensusBudgetCheck {
	chk := NewCensusBudgetCheck()
	chk.Now = func() time.Time { return censusFixtureEnd.Add(30 * time.Second) }
	return chk
}

// writeCensusLog writes a daemon log holding one census record a minute from
// censusLogStart, the rows func returning each record's rows.
func writeCensusLog(t *testing.T, town string, windows int, rows func(i int) []string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < windows; i++ {
		fmt.Fprintf(&b, "%s bd census: total=99 window=60s top: %s\n",
			censusLogStart.Add(time.Duration(i)*time.Minute).Format("2006/01/02 15:04:05"),
			strings.Join(rows(i), " "))
	}
	return writeDoctorTownFile(t, town, "daemon/daemon.log", b.String())
}

// censusDetail returns the detail line naming caller, or "" when none does.
func censusDetail(r *CheckResult, caller string) string {
	for _, d := range r.Details {
		if strings.Contains(d, caller) {
			return d
		}
	}
	return ""
}

// TestCensusBudgetCheckFailsOneCallerOver: a caller above its budget fails the
// check, naming the caller and both numbers.
func TestCensusBudgetCheckFailsOneCallerOver(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeCensusLog(t, town, 60, func(int) []string {
		return []string{
			"list@metrics.queueLandingColumn=20",
			"list@landworker.RetryBeads.List=6",
		}
	})

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusError {
		t.Fatalf("status = %v (%s), want Error", r.Status, r.Message)
	}
	detail := censusDetail(r, "metrics.queueLandingColumn")
	if detail == "" {
		t.Fatalf("details do not name the caller: %q", r.Details)
	}
	for _, want := range []string{"20.0", "12.7"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q must hold the average and the budget (%s)", detail, want)
		}
	}
	if got := censusDetail(r, "landworker.RetryBeads.List"); got != "" {
		t.Errorf("a caller within budget is reported: %q", got)
	}
}

// TestCensusBudgetCheckNotEnoughData: ten windows say nothing about an
// average, so the check reports not enough data rather than passing.
func TestCensusBudgetCheckNotEnoughData(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeCensusLog(t, town, 10, func(int) []string {
		return []string{"list@metrics.queueLandingColumn=1"}
	})

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusSkipped {
		t.Fatalf("status = %v (%s), want Skipped", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not enough data") {
		t.Errorf("message %q must say not enough data", r.Message)
	}
}

// TestCensusBudgetCheckNamesAnUnbudgetedCaller: a caller the budgets file does
// not name is reported as unbudgeted instead of passing silently.
func TestCensusBudgetCheckNamesAnUnbudgetedCaller(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeCensusLog(t, town, 60, func(int) []string {
		return []string{
			"list@metrics.queueLandingColumn=1",
			"list@daemon.newCaller=2",
		}
	})

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want Warning", r.Status, r.Message)
	}
	detail := censusDetail(r, "daemon.newCaller")
	if detail == "" {
		t.Fatalf("details do not name the unbudgeted caller: %q", r.Details)
	}
	if !strings.Contains(detail, "no budget") {
		t.Errorf("detail %q must say the caller has no budget", detail)
	}
}

// TestCensusBudgetCheckNamesACallerTheTableDropped: dropping a row from the
// table turns that caller unbudgeted, while a caller the table keeps and the
// log never shows stays unremarked.
func TestCensusBudgetCheckNamesACallerTheTableDropped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeCensusLog(t, town, 60, func(int) []string {
		return []string{
			"list@metrics.queueLandingColumn=1",
			"list@landworker.RetryBeads.List=1",
		}
	})

	chk := censusCheck()
	chk.Budgets = []beads.CensusBudget{{Caller: "landworker.RetryBeads.List", CallsPerMinute: 6.9}}

	r := chk.Run(&CheckContext{TownRoot: town})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want Warning", r.Status, r.Message)
	}
	detail := censusDetail(r, "metrics.queueLandingColumn")
	if detail == "" || !strings.Contains(detail, "no budget") {
		t.Fatalf("details %q must name the dropped caller as unbudgeted", r.Details)
	}
	if got := censusDetail(r, "landworker.RetryBeads.List"); got != "" {
		t.Errorf("a budgeted caller with no calls in the log is reported: %q", got)
	}
}

// TestCensusBudgetCheckPassesWithinBudget: every caller under its row passes.
func TestCensusBudgetCheckPassesWithinBudget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeCensusLog(t, town, 60, func(int) []string {
		return []string{
			"list@metrics.queueLandingColumn=2",
			"list@landworker.RetryBeads.List=6",
		}
	})

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", r.Status, r.Message)
	}
}

// TestCensusBudgetCheckReadsOnlyTheLogsTail: a log whose newest megabyte holds
// no census record is not measured, whatever older records the file holds.
func TestCensusBudgetCheckReadsOnlyTheLogsTail(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	log := writeCensusLog(t, town, 60, func(int) []string {
		return []string{"list@metrics.queueLandingColumn=99"}
	})
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	noise := strings.Repeat("patrol: nothing to do here\n", int(censusBudgetTailBytes/16))
	if _, err := f.WriteString(noise); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusSkipped || !strings.Contains(r.Message, "not enough data") {
		t.Fatalf("status = %v (%s), want Skipped with not enough data", r.Status, r.Message)
	}
}

// TestCensusBudgetCheckMeasuresOnlyTheNewestWindows: a clean hour no longer
// dilutes a newer burst, because only the newest sixty records are measured.
func TestCensusBudgetCheckMeasuresOnlyTheNewestWindows(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var b strings.Builder
	for i := 0; i < censusBudgetWindows; i++ {
		stamp := censusLogStart.Add(-time.Hour + time.Duration(i)*time.Minute)
		fmt.Fprintf(&b, "%s bd census: total=99 window=60s top: list@metrics.queueLandingColumn=1\n", stamp.Format("2006/01/02 15:04:05"))
	}
	for i := 0; i < censusBudgetWindows; i++ {
		stamp := censusLogStart.Add(time.Duration(i) * time.Minute)
		fmt.Fprintf(&b, "%s bd census: total=99 window=60s top: list@metrics.queueLandingColumn=20\n", stamp.Format("2006/01/02 15:04:05"))
	}
	writeDoctorTownFile(t, town, "daemon/daemon.log", b.String())

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusError {
		t.Fatalf("status = %v (%s), want Error from the newest windows", r.Status, r.Message)
	}
	if detail := censusDetail(r, "metrics.queueLandingColumn"); !strings.Contains(detail, "20.0") {
		t.Errorf("detail %q must measure only the newest windows", detail)
	}
}

// TestCensusBudgetCheckAveragesOverTheSpanNotTheLoggedMinutes: a caller's 60
// calls in one record of an hour-long log average 1 a minute, because the
// quiet minutes the log never wrote still count. Dividing by the records' own
// windows instead charges all 60 to the 21 minutes that logged (2.9 a minute)
// and fails a caller that is inside its budget.
func TestCensusBudgetCheckAveragesOverTheSpanNotTheLoggedMinutes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var b strings.Builder
	for i := 0; i <= 20; i++ {
		rows := "list@daemon.showRigBead=1"
		if i == 10 {
			rows = "list@landworker.RetryBeads.AppendNotes=60"
		}
		fmt.Fprintf(&b, "%s bd census: total=99 window=60s top: %s\n",
			censusLogStart.Add(time.Duration(i)*3*time.Minute).Format("2006/01/02 15:04:05"), rows)
	}
	writeDoctorTownFile(t, town, "daemon/daemon.log", b.String())

	chk := censusCheck()
	chk.Now = func() time.Time { return censusLogStart.Add(time.Hour + 30*time.Second) }
	chk.Budgets = []beads.CensusBudget{
		{Caller: "landworker.RetryBeads.AppendNotes", CallsPerMinute: 1.5},
		{Caller: "daemon.showRigBead", CallsPerMinute: 1.0},
	}

	r := chk.Run(&CheckContext{TownRoot: town})

	if r.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK: 60 calls in an hour average 1 a minute", r.Status, r.Message)
	}
}

// TestCensusBudgetCheckReportsAStaleCensus: a log whose newest record is older
// than five minutes says nothing about the town now, so the check warns
// instead of passing.
func TestCensusBudgetCheckReportsAStaleCensus(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeCensusLog(t, town, 60, func(int) []string {
		return []string{"list@metrics.queueLandingColumn=2"}
	})

	chk := censusCheck()
	chk.Now = func() time.Time { return censusFixtureEnd.Add(11 * time.Minute) }

	r := chk.Run(&CheckContext{TownRoot: town})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want Warning for a stale census", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "census stale: 11m0s") {
		t.Errorf("message %q must say the census is stale and its age", r.Message)
	}
}

// TestCensusBudgetCheckReadsAFreshCensus: a record five minutes old is still
// inside the daemon's cadence, so the check measures it.
func TestCensusBudgetCheckReadsAFreshCensus(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeCensusLog(t, town, 60, func(int) []string {
		return []string{"list@metrics.queueLandingColumn=2"}
	})

	chk := censusCheck()
	chk.Now = func() time.Time { return censusFixtureEnd.Add(censusBudgetStale) }

	if r := chk.Run(&CheckContext{TownRoot: town}); r.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK at the staleness bound", r.Status, r.Message)
	}
}

// TestReadLogTailDropsTheLineTheReadStartsInside: a tail that begins mid-line
// must not hand that fragment to a parser, and a cut at a line boundary drops
// nothing.
func TestReadLogTailDropsTheLineTheReadStartsInside(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		maxBytes int64
	}{
		{"mid-line", 14},
		{"line boundary", 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "log")
			if err := os.WriteFile(path, []byte("FIRST-LINE\nSECOND-LINE\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			lines, err := readLogTail(path, tc.maxBytes)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) == 0 || lines[0] != "SECOND-LINE" {
				t.Fatalf("lines = %q, want the first complete line at or after the cut", lines)
			}
		})
	}
}

// failSeeker is a log the tail read cannot finish: it fails the seek that
// finds the size, the seek that starts the tail, or the read itself.
type failSeeker struct {
	data   []byte
	off    int64
	seeks  int
	failAt int // 1 fails the size seek, 2 the tail seek, 3 the read
}

var errFailSeeker = errors.New("failSeeker: refused")

func (f *failSeeker) Seek(offset int64, whence int) (int64, error) {
	f.seeks++
	if f.failAt == f.seeks {
		return 0, errFailSeeker
	}
	size := int64(len(f.data))
	switch whence {
	case io.SeekStart:
		f.off = offset
	case io.SeekEnd:
		f.off = size + offset
	}
	return f.off, nil
}

func (f *failSeeker) Read(p []byte) (int, error) {
	if f.failAt == 3 {
		return 0, errFailSeeker
	}
	n := copy(p, f.data[f.off:])
	f.off += int64(n)
	return n, nil
}

// TestTailLinesReportsEveryReadFailure: each step of the tail read can fail,
// and each failure reaches the caller instead of yielding a short tail.
func TestTailLinesReportsEveryReadFailure(t *testing.T) {
	t.Parallel()
	for _, failAt := range []int{1, 2, 3} {
		if _, err := tailLines(&failSeeker{data: []byte("one\ntwo\nthree\n"), failAt: failAt}, 8); !errors.Is(err, errFailSeeker) {
			t.Errorf("failAt %d: err = %v, want the read failure", failAt, err)
		}
	}
}

// TestCensusBudgetCheckWarnsOnAnUnreadableLog: a log that exists and cannot be
// read is a fault in the town, not a census that has not run yet, so it warns
// with its own message rather than sharing the no-data skip (gt-gojo0).
func TestCensusBudgetCheckWarnsOnAnUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the log should be: it opens, and reading it fails.
	if err := os.MkdirAll(filepath.Join(town, "daemon", "daemon.log"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want Warning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "cannot read the census") {
		t.Errorf("message %q must say the log could not be read", r.Message)
	}
	if strings.Contains(r.Message, "not enough data") {
		t.Errorf("message %q reads as a census that has not run, not as an unreadable one", r.Message)
	}
}

// TestCensusBudgetCheckSkipsAMissingLog: no daemon log is no data, not a pass,
// and its message names the file so the operator knows which log is missing.
func TestCensusBudgetCheckSkipsAMissingLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusSkipped {
		t.Fatalf("status = %v (%s), want Skipped", r.Status, r.Message)
	}
	if want := filepath.Join(town, "daemon", "daemon.log"); !strings.Contains(r.Message, want) {
		t.Errorf("message %q must name the missing log %s", r.Message, want)
	}
}

// TestCensusBudgetCheckCountsTheRecordsItCannotPlace: a census record written
// above the log's first stamped line has no instant to be averaged over. The
// read leaves it out and says how many it left out, so a rate is never taken
// over records the operator cannot see (gt-gojo0).
func TestCensusBudgetCheckCountsTheRecordsItCannotPlace(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var b strings.Builder
	for i := 0; i < 2; i++ {
		b.WriteString("bd census: total=99 window=60s top: list@metrics.queueLandingColumn=1\n")
	}
	for i := 0; i < censusBudgetWindows; i++ {
		fmt.Fprintf(&b, "%s bd census: total=99 window=60s top: list@metrics.queueLandingColumn=2\n",
			censusLogStart.Add(time.Duration(i)*time.Minute).Format("2006/01/02 15:04:05"))
	}
	writeDoctorTownFile(t, town, "daemon/daemon.log", b.String())

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK: the placed records are within budget", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "2 census record(s) ignored") {
		t.Errorf("details %q must count the two records the read could not place", r.Details)
	}
}

// TestCensusBudgetCheckWarnsOnTornRecords: a census line cut mid-record (it
// carries the marker and does not parse) is counted and named, and a log that
// is within budget only because of what it lost is a warning, never a clean
// pass. The torn lines sit inside the span, so their minutes are counted and
// their calls are not. Mutation: drop the torn count and the status reads OK.
func TestCensusBudgetCheckWarnsOnTornRecords(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var b strings.Builder
	for i := 0; i < 60; i++ {
		stamp := censusLogStart.Add(time.Duration(i) * time.Minute).Format("2006/01/02 15:04:05")
		if i%20 == 7 {
			// Cut after the marker: three windows' worth of rows are gone.
			fmt.Fprintf(&b, "%s bd census: total=99 window=60s top: list@metrics.queueLa\n", stamp)
			continue
		}
		fmt.Fprintf(&b, "%s bd census: total=99 window=60s top: list@landworker.RetryBeads.List=6\n", stamp)
	}
	writeDoctorTownFile(t, town, "daemon/daemon.log", b.String())

	r := censusCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want Warning for a census that lost records", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "3 torn census record(s)") {
		t.Errorf("message = %q, want the three torn records counted", r.Message)
	}
	if censusDetail(r, "torn census record(s) ignored") == "" {
		t.Errorf("details = %q, want the torn records named", r.Details)
	}
}
