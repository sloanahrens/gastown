package daemon

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/question"
	townreport "github.com/steveyegge/gastown/internal/report"
)

var updateOverseerGolden = flag.Bool("update-overseer-golden", false, "rewrite the overseer report golden files")

var overseerTestLoc = time.FixedZone("CDT", -5*3600)

func overseerAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// overseerStore is a fake bd for one store: beadsfake plus the rejection list
// the hour report asks for.
type overseerStore struct{ *beadsfake.Fake }

func (overseerStore) RejectedSince(time.Time) ([]*beads.Issue, error) { return nil, nil }

// overseerWorld is the fixture town the report reads: its stores, seats,
// dispatcher answer and health line, each replaceable by a failing one.
type overseerWorld struct {
	town   string
	now    time.Time
	stores map[string]*overseerStore
	seats  func() ([]townreport.SeatWork, error)
	left   func() (townreport.DispatchLeft, error)
	health func() (string, error)
	rigs   func() ([]string, error)
	logf   func(string, ...any)
}

func newOverseerWorld(t *testing.T, now string) *overseerWorld {
	t.Helper()
	town := t.TempDir()
	// The hour sections read the attention state; without a file their row
	// would name the temp directory.
	writeOverseerFile(t, filepath.Join(town, ".runtime", "attention", "state.json"),
		`{"updated":"2026-10-14T20:59:50Z","items":[]}`)

	hq, gastown := beadsfake.New(), beadsfake.New()
	hq.Seed(beads.Issue{
		ID: "hq-q1", Title: "Retire the cron prompt?", Status: "open",
		Labels:    []string{question.Label},
		CreatedAt: "2026-10-14T17:05:00Z",
		Description: "## Question\nShould the cron prompt go?\n\n## Recommended answer\nYes.\n\n" +
			"## Held work\n- gt-hold1 first\n- gt-hold2 second\n",
	}, beads.Issue{
		ID: "hq-q2", Title: "Which seat class?", Status: "open",
		Labels:      []string{question.Label},
		CreatedAt:   "2026-10-14T19:40:00Z",
		Description: "## Question\nWhich?\n\n## Recommended answer\nThe pool.\n\n## Held work\nnone\n",
	})
	gastown.Seed(
		beads.Issue{ID: "gt-done1", Title: "Add the thing", Status: "closed", ClosedAt: "2026-10-14T20:03:00Z"},
		beads.Issue{ID: "gt-done2", Title: "Fix the other\nthing", Status: "closed", ClosedAt: "2026-10-14T20:10:00Z"},
		beads.Issue{ID: "gt-before", Title: "Closed before the window", Status: "closed", ClosedAt: "2026-10-14T19:59:59Z"},
		beads.Issue{ID: "gt-after", Title: "Closed after the slot", Status: "closed", ClosedAt: "2026-10-14T20:15:30Z"},
		beads.Issue{ID: "gt-wip1", Title: "Work on the thing", Status: "hooked"},
	)
	return &overseerWorld{
		town: town,
		now:  overseerAt(now),
		stores: map[string]*overseerStore{
			"hq":      {hq},
			"gastown": {gastown},
		},
		seats: func() ([]townreport.SeatWork, error) {
			return []townreport.SeatWork{{Rig: "gastown", Polecat: "nux", Bead: "gt-wip1", Since: overseerAt("2026-10-14T20:03:33Z")}}, nil
		},
		left: func() (townreport.DispatchLeft, error) {
			return overseerReportLeftFrom(specDispatchTickReport{
				Dispatched: []specDispatchTickEntry{{Bead: "gt-n1", Rig: "gastown", Line: "gt-n1: would sling to gastown on claude-sonnet (free seat)"}},
				Skipped: []specDispatchTickEntry{
					{Bead: "gt-n2", Rig: "gastown", Line: "gt-n2: clean, per-tick limit 1 reached"},
					{Bead: "gt-n3", Rig: "gastown", Line: "gt-n3: no seat: claude-sonnet 2/2"},
					{Bead: "gt-n4", Rig: "gastown", Line: "gt-n4: clean, per-tick limit 1 reached"},
					{Bead: "gt-u1", Rig: "gastown", Line: "gt-u1: unshaped: missing ## Gate"},
				},
				Planning: []specDispatchTickEntry{{Bead: "gt-p1", Rig: "gastown", Line: "gt-p1: needs plan"}},
			})
		},
		health: func() (string, error) { return "GREEN tick 12s ago, 3 landed/24h", nil },
		rigs:   func() ([]string, error) { return []string{"gastown"}, nil },
		// The report's own log is only about the latest.md copy; a test that
		// wants those lines sets its own.
		logf: func(string, ...any) {},
	}
}

func writeOverseerFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *overseerWorld) store(name string) (townreport.BeadStore, error) {
	s, ok := w.stores[name]
	if !ok {
		return nil, fmt.Errorf("no beads directory for %s", name)
	}
	return s, nil
}

func (w *overseerWorld) seams() *overseerReportSeams {
	return &overseerReportSeams{
		now:  func() time.Time { return w.now },
		logf: w.logf,
		digest: func(slot, since time.Time) townreport.Digest {
			return townreport.Digest{
				Now: w.now, Slot: slot, Since: since,
				Rigs: w.rigs, Store: w.store,
				Seats: w.seats, Left: w.left, Health: w.health,
			}
		},
		hour: func(slot time.Time) townreport.HourRun {
			return townreport.HourRun{
				TownRoot: w.town,
				Loc:      overseerTestLoc,
				Until:    slot,
				Version:  "daemon",
				Commit:   "5e6f7a8b5e6f7a8b5e6f7a8b5e6f7a8b5e6f7a8b",
				Rigs:     w.rigs,
				Store:    w.store,
				MainTip: func(string) (townreport.Mains, error) {
					return townreport.Mains{SHA: "138368fd7571c0ffee00", CommitsBehind: 3}, nil
				},
			}
		},
	}
}

// seedReport plants an earlier report file, as a previous run would have left.
func (w *overseerWorld) seedReport(t *testing.T, stamp string) string {
	t.Helper()
	path := filepath.Join(townreport.ReportsDir(w.town), stamp+".md")
	writeOverseerFile(t, path, "earlier report\n")
	return path
}

func (w *overseerWorld) run(t *testing.T) string {
	t.Helper()
	path, err := writeOverseerReport(w.town, 15*time.Minute, w.seams())
	if err != nil {
		t.Fatalf("writeOverseerReport: %v", err)
	}
	return path
}

func checkOverseerGolden(t *testing.T, name, got string) {
	t.Helper()
	golden := filepath.Join("testdata", name)
	if *updateOverseerGolden {
		writeOverseerFile(t, golden, got)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading %s: %v", golden, err)
	}
	if got != string(want) {
		t.Fatalf("report differs from %s:\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

// overseerEntries names what a run left in the reports directory, sorted, so a
// test says exactly that.
func overseerEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func readOverseerFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestOverseerReportQuarterMatchesGolden(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")

	path := w.run(t)
	if filepath.Base(path) != "20261014T201500Z.md" {
		t.Fatalf("report file = %q, want the slot's timestamp name", filepath.Base(path))
	}
	text := readOverseerFile(t, path)
	checkOverseerGolden(t, "overseer_report_quarter.md", text)
	if strings.Contains(text, "\nHOUR\n") {
		t.Errorf("a quarter-hour report carries the hour sections:\n%s", text)
	}
}

func TestOverseerReportHourAddsTheHourSections(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T21:02:10Z")
	w.seedReport(t, "20261014T204500Z")

	text := readOverseerFile(t, w.run(t))
	checkOverseerGolden(t, "overseer_report_hour.md", text)
	for _, want := range []string{"\nHOUR\n", "hour ending ", "\nlandings\n", "\nattention\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("hourly report is missing %q", want)
		}
	}
}

// The first report written after the clock crosses an hour carries the hour
// sections even when the :00 slot was missed, and the one after it does not.
func TestOverseerReportHourFollowsThePreviousReportNotTheMinute(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T21:20:00Z")
	w.seedReport(t, "20261014T204500Z")
	if text := readOverseerFile(t, w.run(t)); !strings.Contains(text, "\nHOUR\n") {
		t.Errorf("the first report of the 21:00 hour lacks the hour sections:\n%s", text)
	}

	w.now = overseerAt("2026-10-14T21:35:00Z")
	if text := readOverseerFile(t, w.run(t)); strings.Contains(text, "\nHOUR\n") {
		t.Errorf("the second report of the hour carries the hour sections:\n%s", text)
	}
}

func TestOverseerReportSecondRunInASlotWritesNothing(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:15:01Z")
	first := w.run(t)
	if first == "" {
		t.Fatal("the first run wrote nothing")
	}
	body := readOverseerFile(t, first)

	w.now = overseerAt("2026-10-14T20:29:59Z")
	if second := w.run(t); second != "" {
		t.Fatalf("a second run in the slot wrote %s", second)
	}
	if got := overseerEntries(t, townreport.ReportsDir(w.town)); !slices.Equal(got, []string{"20261014T201500Z.md", overseerLatestName}) {
		t.Errorf("reports dir holds %q, want the one report, the latest.md copy and no leftover temp file", got)
	}
	if readOverseerFile(t, first) != body {
		t.Error("the slot's report was rewritten")
	}

	w.now = overseerAt("2026-10-14T20:30:00Z")
	if next := w.run(t); filepath.Base(next) != "20261014T203000Z.md" {
		t.Errorf("the next slot's report = %q", next)
	}
}

// A report the patrol did not write, or one stamped past the slot, is never
// replaced or followed by an older one.
func TestOverseerReportNeverReplacesAnExistingFile(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:00Z")
	slotFile := w.seedReport(t, "20261014T201500Z")
	if path := w.run(t); path != "" {
		t.Errorf("wrote %s over the slot's existing report", path)
	}
	if got := readOverseerFile(t, slotFile); got != "earlier report\n" {
		t.Errorf("existing report now reads %q", got)
	}

	w2 := newOverseerWorld(t, "2026-10-14T20:17:00Z")
	w2.seedReport(t, "20261014T203000Z")
	if path, _ := writeOverseerReport(w2.town, 15*time.Minute, w2.seams()); path != "" {
		t.Errorf("wrote %s although a later report exists", path)
	}
}

// A report file named for an instant the clock has not reached holds the
// patrol off, and the run says so instead of returning empty and silent.
func TestWriteOverseerReportFutureFileStillWrites(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:00Z")
	future := w.seedReport(t, "20261014T230000Z")

	path, err := writeOverseerReport(w.town, 15*time.Minute, w.seams())
	if path != "" {
		t.Errorf("wrote %s under a future-dated report", path)
	}
	if err == nil || !strings.Contains(err.Error(), "20261014T230000Z") || !strings.Contains(err.Error(), "dated after now") {
		t.Errorf("err = %v, want it to name the future-dated report", err)
	}
	if got := readOverseerFile(t, future); got != "earlier report\n" {
		t.Errorf("the future-dated report was changed to %q", got)
	}

	// A report for the slot the clock is in is covered, not a future one.
	w.now = overseerAt("2026-10-14T23:05:00Z")
	if path, err := writeOverseerReport(w.town, 15*time.Minute, w.seams()); path != "" || err != nil {
		t.Errorf("run in the covered slot = %q, %v, want nothing and no error", path, err)
	}
}

func TestOverseerReportTickEveryFiresSeveralTimesASlot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ interval, want time.Duration }{
		{15 * time.Minute, 225 * time.Second},
		{time.Hour, 15 * time.Minute},
		{3 * time.Second, 3 * time.Second},
	} {
		if got := overseerReportTickEvery(tc.interval); got != tc.want {
			t.Errorf("overseerReportTickEvery(%v) = %v, want %v", tc.interval, got, tc.want)
		}
	}
}

func TestOverseerReportFailingSourcesAreOneLineEach(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")
	w.seats = func() ([]townreport.SeatWork, error) { return nil, errors.New("tmux is down") }
	w.left = func() (townreport.DispatchLeft, error) {
		return townreport.DispatchLeft{}, errors.New("gt spec dispatch: exit 1")
	}
	w.health = func() (string, error) { return "", errors.New("health file missing") }
	delete(w.stores, "hq")

	path := w.run(t)
	if path == "" {
		t.Fatal("no report was written although only sources failed")
	}
	text := readOverseerFile(t, path)
	for _, want := range []string{
		"IN PROGRESS\n  unavailable (tmux is down)\n",
		"LEFT\n  unavailable (gt spec dispatch: exit 1)\n",
		"OPEN QUESTIONS\n  unavailable (no beads directory for hq)\n",
		"health: unavailable (health file missing)\n",
		"gt-done1  Add the thing  gastown",
		"unavailable (hq: no beads directory for hq)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "unavailable ("); n != 5 {
		t.Errorf("report has %d unavailable lines, want one per failed source (5):\n%s", n, text)
	}
}

func TestOverseerReportNameIsReadByTheDashboard(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")
	w.run(t)

	got := dashboard.NewReportsReader(townreport.ReportsDir(w.town)).Read(w.now)
	if got.State != dashboard.ReportsFresh {
		t.Fatalf("dashboard reads the reports as %q (%s), want fresh", got.State, got.Note)
	}
	if want := overseerAt("2026-10-14T20:15:00Z"); !got.Written.Equal(want) {
		t.Errorf("dashboard dates the report %v, want the slot %v", got.Written, want)
	}
	if !strings.Contains(got.Text, "overseer report 2026-10-14T20:15:00Z") {
		t.Errorf("dashboard shows %q, want the new report's text", got.Text)
	}
	if len(got.Times) != 2 {
		t.Errorf("dashboard lists %d reports, want the seeded one and the new one", len(got.Times))
	}
}

// A digest refreshes latest.md from the same text, so a reader that takes the
// newest report by name gets the newest one and not whatever a human wrote
// there last.
func TestOverseerReportDigestRefreshesLatestCopy(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")
	dir := townreport.ReportsDir(w.town)
	copyPath := filepath.Join(dir, overseerLatestName)
	writeOverseerFile(t, copyPath, "the 10-15 copy\n")
	held, err := os.Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	text := readOverseerFile(t, w.run(t))
	if got := readOverseerFile(t, copyPath); got != text {
		t.Errorf("latest.md holds\n%s\nwant the report it copies\n%s", got, text)
	}
	// The copy is replaced, not written through: a reader that opened the old
	// one keeps it whole, which is what the temp file and the rename buy.
	old, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "the 10-15 copy\n" {
		t.Errorf("a reader holding the old copy now sees %q, want the text it opened", old)
	}
	before, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, now) {
		t.Error("latest.md was written in place, so a reader can catch it half-written")
	}
	if got := overseerEntries(t, dir); !slices.Equal(got, []string{"20261014T200000Z.md", "20261014T201500Z.md", overseerLatestName}) {
		t.Errorf("reports dir holds %q, want the two reports, the copy and no leftover temp file", got)
	}
}

// latest.md is a copy, not a report: a directory holding only it has no report,
// so the run writes the slot's report with a first report's window, and the
// previous-report lookup still reads the digests around it.
func TestOverseerReportLatestCopyIsNotAReport(t *testing.T) {
	t.Parallel()
	if overseerReportNameRe.MatchString(overseerLatestName) {
		t.Fatalf("overseerReportNameRe matches %s, so the lister reads the copy as a digest", overseerLatestName)
	}
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	dir := townreport.ReportsDir(w.town)
	writeOverseerFile(t, filepath.Join(dir, overseerLatestName), "the 10-15 copy\n")

	if prev, newest, err := overseerReportPrevious(dir, overseerAt("2026-10-14T20:15:00Z")); err != nil || !prev.IsZero() || !newest.IsZero() {
		t.Errorf("overseerReportPrevious over a directory holding only %s = %v, %v, %v; want no reports", overseerLatestName, prev, newest, err)
	}
	text := readOverseerFile(t, w.run(t))
	if !strings.Contains(text, "covers 2026-10-14T20:00:00Z to 2026-10-14T20:15:00Z") {
		t.Errorf("the copy stood in for a previous report:\n%s", text)
	}

	// The copy sits beside the new digest and bounds nothing: the next slot's
	// previous report is the digest.
	w.now = overseerAt("2026-10-14T20:32:00Z")
	next := readOverseerFile(t, w.run(t))
	if !strings.Contains(next, "covers 2026-10-14T20:15:00Z to 2026-10-14T20:30:00Z") {
		t.Errorf("the copy was read as the previous report:\n%s", next)
	}
}

// A latest.md the run cannot replace is logged, and the slot's report stands:
// the copy is for a reader by hand, the report is the record.
func TestOverseerReportLatestCopyFailureIsLogged(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")
	var logs []string
	w.logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	dir := townreport.ReportsDir(w.town)
	// A directory where the copy goes: the rename cannot replace it.
	if err := os.MkdirAll(filepath.Join(dir, overseerLatestName), 0o755); err != nil {
		t.Fatal(err)
	}

	path := w.run(t)
	if path == "" {
		t.Fatal("no report was written although only the copy failed")
	}
	if text := readOverseerFile(t, path); !strings.Contains(text, "overseer report 2026-10-14T20:15:00Z") {
		t.Errorf("the report is not the slot's digest:\n%s", text)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], overseerLatestName) {
		t.Errorf("logged %q, want one line naming %s", logs, overseerLatestName)
	}
	if fi, err := os.Stat(filepath.Join(dir, overseerLatestName)); err != nil || !fi.IsDir() {
		t.Errorf("stat of the copy = %v, %v; want the directory the run could not replace", fi, err)
	}
	if got := overseerEntries(t, dir); !slices.Equal(got, []string{"20261014T200000Z.md", "20261014T201500Z.md", overseerLatestName}) {
		t.Errorf("reports dir holds %q, want the reports, the directory and no leftover temp file", got)
	}
}

// A report name the run finds taken is the one case the writer leaves without
// refreshing latest.md, and it says nothing: no report came from this run, so
// the copy keeps the text of the report that did, and the run is not a failed
// refresh to log.
func TestOverseerReportTakenNameKeepsTheLatestCopy(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	dir := townreport.ReportsDir(w.town)
	slotFile := w.seedReport(t, "20261014T201500Z")
	copyPath := filepath.Join(dir, overseerLatestName)
	writeOverseerFile(t, copyPath, "the 10-15 copy\n")
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	path, err := writeOverseerReportFile(dir, overseerAt("2026-10-14T20:15:00Z"), "the slot's text\n", logf)
	if path != "" || err != nil {
		t.Fatalf("writeOverseerReportFile over a taken report name = %q, %v; want nothing and no error", path, err)
	}
	if got := readOverseerFile(t, slotFile); got != "earlier report\n" {
		t.Errorf("the report in place now reads %q", got)
	}
	if got := readOverseerFile(t, copyPath); got != "the 10-15 copy\n" {
		t.Errorf("latest.md now reads %q, want the text the run left alone", got)
	}
	if len(logs) != 0 {
		t.Errorf("logged %q, want nothing: the refresh was skipped, not attempted and failed", logs)
	}
	if got := overseerEntries(t, dir); !slices.Equal(got, []string{"20261014T201500Z.md", overseerLatestName}) {
		t.Errorf("reports dir holds %q, want the report, the copy and no leftover temp file", got)
	}
}

// A nil logf is what a hand-built seam may carry, and the report holds: the
// patrol runs the report on its own goroutine, so a panic there over a copy
// nobody read is a patrol that stops reporting.
func TestOverseerReportMissingLoggerIsNotAPanic(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")
	// A directory where the copy goes: the rename cannot replace it, which is
	// the failure the missing logger is asked to report.
	if err := os.MkdirAll(filepath.Join(townreport.ReportsDir(w.town), overseerLatestName), 0o755); err != nil {
		t.Fatal(err)
	}
	w.logf = nil

	path := w.run(t)
	if path == "" {
		t.Fatal("no report was written although only the copy failed")
	}
	if text := readOverseerFile(t, path); !strings.Contains(text, "overseer report 2026-10-14T20:15:00Z") {
		t.Errorf("the report is not the slot's digest:\n%s", text)
	}
}

// The window DONE counts is the previous report to the slot, and the first
// report has a window of one interval.
func TestOverseerReportDoneCountsSincePreviousReport(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	text := readOverseerFile(t, w.run(t))
	if !strings.Contains(text, "covers 2026-10-14T20:00:00Z to 2026-10-14T20:15:00Z") {
		t.Errorf("first report's window is not one interval:\n%s", text)
	}

	w2 := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w2.seedReport(t, "20261014T194500Z")
	text = readOverseerFile(t, w2.run(t))
	if !strings.Contains(text, "gt-before") {
		t.Errorf("gt-before closed after the previous report (19:45) but is missing:\n%s", text)
	}
	if strings.Contains(text, "gt-after") {
		t.Errorf("gt-after closed after the slot and belongs to the next report:\n%s", text)
	}
}

func TestOverseerReportStaysUnderSixtyLines(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")
	for i := 0; i < 40; i++ {
		w.stores["gastown"].Seed(beads.Issue{ID: fmt.Sprintf("gt-bulk%02d", i), Title: "Bulk", Status: "closed", ClosedAt: "2026-10-14T20:05:00Z"})
		w.stores["hq"].Seed(beads.Issue{
			ID: fmt.Sprintf("hq-bulkq%02d", i), Title: "Q", Status: "open", Labels: []string{question.Label},
			CreatedAt: "2026-10-14T18:00:00Z", Description: "## Question\nq\n## Recommended answer\na\n",
		})
	}
	w.seats = func() ([]townreport.SeatWork, error) {
		var seats []townreport.SeatWork
		for i := 0; i < 30; i++ {
			seats = append(seats, townreport.SeatWork{Rig: "gastown", Polecat: fmt.Sprintf("p%02d", i), Bead: "gt-wip1"})
		}
		return seats, nil
	}
	w.left = func() (townreport.DispatchLeft, error) {
		var l townreport.DispatchLeft
		for i := 0; i < 30; i++ {
			l.Next = append(l.Next, townreport.DispatchRow{Bead: fmt.Sprintf("gt-n%02d", i), Rig: "gastown", Text: "would sling"})
			l.Held = append(l.Held, townreport.DispatchRow{Bead: fmt.Sprintf("gt-h%02d", i), Rig: "gastown", Text: "unshaped"})
		}
		return l, nil
	}
	text := readOverseerFile(t, w.run(t))
	if n := strings.Count(text, "\n"); n >= townreport.DigestMaxLines {
		t.Errorf("digest runs to %d lines, want under %d:\n%s", n, townreport.DigestMaxLines, text)
	}
	for _, want := range []string{"+30 more", "+20 more", "+24 more", "+36 more"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest does not say what it left out (%q):\n%s", want, text)
		}
	}
}

// A bead with a sling in flight is that sling's, not the next tick's: the LEFT
// dry run runs with the same in-flight set the real tick does, so the bead is
// not listed as next and its seat is not counted free (gt-nosod).
func TestOverseerReportLeftPassesInFlight(t *testing.T) {
	t.Parallel()
	var args []string
	fake := newFakeCLI(func(a []string) cliReply {
		args = a
		// The dispatcher leaves an in-flight bead off its next list and counts
		// the seat as taken; without the flag it would offer both.
		if slices.Contains(a, "--in-flight") {
			return cliReply{stdout: string(wireOut(`{"roster": "claude-sonnet 1/2", "candidates": 0}`))}
		}
		return cliReply{stdout: string(wireOut(`{"roster": "claude-sonnet 0/2", "candidates": 1,
			"dispatched": [{"bead": "gt-a", "rig": "gastown", "line": "gt-a: would sling to gastown on claude-sonnet"}]}`))}
	})
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, ctx: t.Context(), logger: discardLogger, gtPath: "gt", execCmd: fake.run}
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", Rig: "gastown", started: time.Now()})

	left, err := d.overseerReportLeft()
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(args, "--dry-run") || !slices.Contains(args, "--in-flight") {
		t.Fatalf("dry run args = %v, want the tick's --in-flight and --dry-run", args)
	}
	if i := slices.Index(args, "--in-flight"); i+1 >= len(args) || !strings.Contains(args[i+1], `"gt-a"`) {
		t.Errorf("--in-flight = %v, want it to name gt-a", args)
	}
	if slices.ContainsFunc(left.Next, func(r townreport.DispatchRow) bool { return r.Bead == "gt-a" }) {
		t.Errorf("LEFT Next = %+v, want the in-flight bead left out", left.Next)
	}
}

// A set of running slings that cannot be written down fails the LEFT read; a
// dry run without --in-flight would list the running bead as next and count
// its seat free, the reading the flag exists to prevent (gt-nosod).
func TestOverseerReportLeftFailsWhenTheInFlightSetCannotBeWritten(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: string(wireOut(`{"roster": "claude-sonnet 0/2"}`))}
	})
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, ctx: t.Context(), logger: discardLogger, gtPath: "gt", execCmd: fake.run}
	// A year time.Time cannot marshal is the one way the in-flight payload
	// fails to write.
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", Rig: "gastown", started: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})

	if _, err := d.overseerReportLeft(); err == nil {
		t.Fatal("overseerReportLeft returned no error for an in-flight set that cannot be written")
	}
	if got := fake.argvs("spec", "dispatch"); len(got) != 0 {
		t.Errorf("dry runs = %v, want none without the flag", got)
	}
}

func TestOverseerReportLeftSortsTheDryRun(t *testing.T) {
	t.Parallel()
	left, err := overseerReportLeftFrom(specDispatchTickReport{
		Dispatched: []specDispatchTickEntry{{Bead: "gt-a", Rig: "r", Line: "gt-a: would sling to r on x (y)"}},
		Skipped: []specDispatchTickEntry{
			{Bead: "gt-b", Rig: "r", Line: "gt-b: no seat: claude 2/2"},
			{Bead: "gt-c", Rig: "r", Line: "gt-c: unshaped: missing ## Gate"},
			{Bead: "gt-d", Rig: "r", Line: "gt-d: clean, per-tick limit 1 reached"},
		},
		Refused: []specDispatchTickEntry{{Bead: "gt-e", Rig: "r", Line: "gt-e: refused by lint"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var next, held []string
	for _, r := range left.Next {
		next = append(next, r.Bead)
	}
	for _, r := range left.Held {
		held = append(held, r.Bead+" "+r.Text)
	}
	if got := strings.Join(next, ","); got != "gt-a,gt-b,gt-d" {
		t.Errorf("next = %s, want the would-sling bead then the clean ones waiting on a seat", got)
	}
	if got := strings.Join(held, "|"); got != "gt-c unshaped: missing ## Gate|gt-e refused: refused by lint" {
		t.Errorf("held = %s", got)
	}
	if held, err := overseerReportLeftFrom(specDispatchTickReport{Hold: "operator hold"}); err != nil || held.Hold != "operator hold" {
		t.Errorf("hold = %q, err = %v", held.Hold, err)
	}
}

// A dry run that exits 0 with Errors set (an unreadable roster, a failed board
// read) decided nothing about the beads it could not read, so LEFT reads as
// unavailable rather than as an empty queue.
func TestOverseerReportLeftDryRunErrors(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.left = func() (townreport.DispatchLeft, error) {
		return overseerReportLeftFrom(specDispatchTickReport{Errors: []string{"roster unreadable", "gastown: board read failed"}})
	}
	slot := overseerAt("2026-10-14T20:15:00Z")
	text := w.seams().digest(slot, slot.Add(-15*time.Minute)).Render()

	want := "LEFT\n  unavailable (dispatch dry run reported errors: roster unreadable (+1 more))\n"
	if !strings.Contains(text, want) {
		t.Errorf("LEFT does not say the dry run failed (want %q):\n%s", want, text)
	}
	if strings.Contains(text, "LEFT\n  (none)") {
		t.Errorf("LEFT reads as an empty queue:\n%s", text)
	}
}

// A dry run that decided some beads and failed to read another keeps the
// decided rows and says how many errors it carried; only a run that decided
// nothing is unavailable.
func TestOverseerReportLeftPartialErrorsKeepRows(t *testing.T) {
	t.Parallel()
	left, err := overseerReportLeftFrom(specDispatchTickReport{
		Dispatched: []specDispatchTickEntry{{Bead: "gt-a", Rig: "r", Line: "gt-a: would sling to r on x (y)"}},
		Skipped:    []specDispatchTickEntry{{Bead: "gt-c", Rig: "r", Line: "gt-c: unshaped: missing ## Gate"}},
		Errors:     []string{"gt-z: bd show timed out"},
	})
	if err != nil {
		t.Fatalf("a partial failure made LEFT unavailable: %v", err)
	}
	if len(left.Next) != 1 || len(left.Held) != 1 || left.Errors != 1 {
		t.Fatalf("left = %+v, want one next, one held and one error", left)
	}

	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.left = func() (townreport.DispatchLeft, error) { return left, nil }
	slot := overseerAt("2026-10-14T20:15:00Z")
	text := w.seams().digest(slot, slot.Add(-15*time.Minute)).Render()
	for _, want := range []string{"next  gt-a", "held  gt-c", "reported 1 error(s)"} {
		if !strings.Contains(text, want) {
			t.Errorf("LEFT lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "unavailable (dispatch dry run") {
		t.Errorf("LEFT reads as unavailable:\n%s", text)
	}
}

// A bead pinned to an agent the pool has no seat for never gets one, so it is
// held, where a bead waiting on a full seat is next.
func TestOverseerReportLeftUnservablePinHeld(t *testing.T) {
	t.Parallel()
	left, err := overseerReportLeftFrom(specDispatchTickReport{
		Skipped: []specDispatchTickEntry{
			{Bead: "gt-p", Rig: "r", Line: "gt-p: no seat: pinned to claude-opus, which has no seat in the pool: claude 2/2"},
			{Bead: "gt-w", Rig: "r", Line: "gt-w: no seat: waiting for a claude seat: claude 2/2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Held) != 1 || left.Held[0].Bead != "gt-p" {
		t.Errorf("held = %+v, want the unservable pin", left.Held)
	}
	if len(left.Next) != 1 || left.Next[0].Bead != "gt-w" {
		t.Errorf("next = %+v, want the bead waiting on a full seat", left.Next)
	}
}

// A bead the dispatcher holds out with the spec-dispatch-failed label has no
// skip entry, only the LabeledFailed count. With it the only ready bead, LEFT
// must still say something is held, not "(none)".
func TestOverseerReportLeftLabeledFailed(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.left = func() (townreport.DispatchLeft, error) {
		return overseerReportLeftFrom(specDispatchTickReport{LabeledFailed: 1})
	}
	slot := overseerAt("2026-10-14T20:15:00Z")
	text := w.seams().digest(slot, slot.Add(-15*time.Minute)).Render()

	want := "LEFT\n  held  1 by label " + constants.LabelSpecDispatchFailed
	if !strings.Contains(text, want) {
		t.Errorf("LEFT does not say how many beads the label holds (want %q):\n%s", want, text)
	}
	if strings.Contains(text, "LEFT\n  (none)") {
		t.Errorf("LEFT reads as an empty queue:\n%s", text)
	}
}

func TestOverseerReportPatrolIsOffUntilEnabled(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	if d.isPatrolActive("overseer_report") {
		t.Error("overseer_report is active with no config")
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverseerReport: &OverseerReportConfig{}}}
	if d.isPatrolActive("overseer_report") {
		t.Error("overseer_report is active with enabled unset")
	}
	d.patrolConfig.Patrols.OverseerReport.Enabled = true
	if !d.isPatrolActive("overseer_report") {
		t.Error("overseer_report is off although enabled")
	}
	d.disabledPatrols = map[string]bool{"overseer_report": true}
	if d.isPatrolActive("overseer_report") {
		t.Error("the town's disabled_patrols list does not turn overseer_report off")
	}
}

func TestOverseerReportIntervalDefaultsToFifteenMinutes(t *testing.T) {
	t.Parallel()
	if got := overseerReportInterval(nil); got != 15*time.Minute {
		t.Errorf("default interval = %v", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverseerReport: &OverseerReportConfig{Enabled: true, IntervalStr: "5m"}}}
	if got := overseerReportInterval(cfg); got != 5*time.Minute {
		t.Errorf("configured interval = %v", got)
	}
	cfg.Patrols.OverseerReport.IntervalStr = "nonsense"
	if got := overseerReportInterval(cfg); got != 15*time.Minute {
		t.Errorf("bad interval = %v, want the default", got)
	}
}

// The ticker's trigger writes the slot's report on its own goroutine, once at
// a time, and only while the patrol is on.
func TestTriggerOverseerReport(t *testing.T) {
	t.Parallel()
	w := newOverseerWorld(t, "2026-10-14T20:17:33Z")
	w.seedReport(t, "20261014T200000Z")
	d := &Daemon{
		config:              &Config{TownRoot: w.town},
		logger:              log.New(io.Discard, "", 0),
		overseerReportSeams: w.seams(),
	}

	slotFile := filepath.Join(townreport.ReportsDir(w.town), "20261014T201500Z.md")

	d.triggerOverseerReport()
	d.overseerReportCycles.Wait()
	if _, err := os.Stat(slotFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a disabled patrol wrote a report (stat err %v)", err)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverseerReport: &OverseerReportConfig{Enabled: true}}}
	d.overseerReportRunning.Store(true)
	d.triggerOverseerReport()
	d.overseerReportCycles.Wait()
	if _, err := os.Stat(slotFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a second run started while one was running (stat err %v)", err)
	}
	d.overseerReportRunning.Store(false)
	d.triggerOverseerReport()
	d.overseerReportCycles.Wait()
	if d.overseerReportRunning.Load() {
		t.Fatal("the single-flight guard was not released")
	}
	if _, err := os.Stat(slotFile); err != nil {
		t.Fatalf("the enabled patrol wrote no report: %v", err)
	}
}
