package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// landOmStatsFailIO fails every read and write, standing in for a log whose
// read fails and an output that refuses the JSON report.
type landOmStatsFailIO struct{ err error }

func (f landOmStatsFailIO) Read([]byte) (int, error)  { return 0, f.err }
func (f landOmStatsFailIO) Write([]byte) (int, error) { return 0, f.err }

// landOmStatsWireFS serves empty records and a log whose read fails, so a test
// reaches land.AggregateOmStats's own error rather than an open failure.
type landOmStatsWireFS struct{}

func (landOmStatsWireFS) Open(name string) (io.ReadCloser, error) {
	if name == "/rec" {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return io.NopCloser(landOmStatsFailIO{err: errors.New("the log cannot be read")}), nil
}

// TestLandOmStatsRecurrenceFollowCounts: generated held-out and filed beads,
// each with one minor finding, are restated in time by a later record of another
// bead, so each side tallies two findings and follows both.
func TestLandOmStatsRecurrenceFollowCounts(t *testing.T) {
	t.Parallel()
	class, in := omlessons.HoldOutClasses()[0], omlessons.HoldOutStart().Add(time.Hour)
	now := in.Add(2 * time.Hour)
	doc := land.ReviewFindingText{Severity: "minor", File: "docs/a.md", Title: "the documentation says the flag defaults to true"}
	restated := land.ReviewFindingText{Severity: "major", File: "docs/a.md", Title: "the documentation says the flag defaults to true"}
	var heldBeads, filedBeads []string
	for i := 0; i < 200 && (len(heldBeads) < 2 || len(filedBeads) < 2); i++ {
		bead := fmt.Sprintf("gt-rec-%04d", i)
		if omlessons.HeldOut(bead, class, in) {
			if len(heldBeads) < 2 {
				heldBeads = append(heldBeads, bead)
			}
		} else if len(filedBeads) < 2 {
			filedBeads = append(filedBeads, bead)
		}
	}
	if len(heldBeads) < 2 || len(filedBeads) < 2 {
		t.Fatalf("generated %d held-out and %d filed beads, want two of each", len(heldBeads), len(filedBeads))
	}
	var records []land.ReviewRecord
	for _, bead := range append(heldBeads, filedBeads...) {
		records = append(records,
			land.ReviewRecord{BeadID: bead, At: in, FindingText: []land.ReviewFindingText{doc}},
			land.ReviewRecord{BeadID: bead + "-later", At: in.Add(time.Hour), FindingText: []land.ReviewFindingText{restated}})
	}
	row := recurrence(records, now)[0]
	if row.ID != class || row.HeldOut.Findings != 2 || row.HeldOut.Followed != 2 || row.Filed.Findings != 2 || row.Filed.Followed != 2 {
		t.Errorf("row = %+v, want %q with two held out and two filed, all followed", row, class)
	}
}

// TestLandOmStatsRecurrenceFollowExcludes: an in-time other-bead restatement is
// the positive control, and each other row changes one thing that must keep it
// from counting: a record at the same instant, the same bead, a restatement
// after 28 days, a restatement on another file, and a restating record after
// now. Every row tallies the base as one held-out finding, and only the
// other-file row moves the restatement off the base's file, so no other zero
// comes from a missing base or a file mismatch.
func TestLandOmStatsRecurrenceFollowExcludes(t *testing.T) {
	t.Parallel()
	class, in := omlessons.HoldOutClasses()[0], omlessons.HoldOutStart().Add(time.Hour)
	const held, other = "gt-rec-0004", "gt-rec-0009"
	if !omlessons.HeldOut(held, class, in) || held == other {
		t.Fatalf("fixture beads %s and %s are not distinct with %s held out", held, other, held)
	}
	base := land.ReviewFindingText{Severity: "minor", File: "internal/cmd/foo.go", Title: "the documentation says the flag defaults to true"}
	restated := land.ReviewFindingText{Severity: "major", File: "internal/cmd/foo.go", Title: "the documentation says the flag defaults to true"}
	elsewhere := land.ReviewFindingText{Severity: "major", File: "internal/cmd/bar.go", Title: "the documentation says the flag defaults to true"}
	inTime := in.Add(time.Hour)
	records := func(candidate land.ReviewRecord) []land.ReviewRecord {
		return []land.ReviewRecord{
			{BeadID: held, At: in, FindingText: []land.ReviewFindingText{base}},
			candidate,
		}
	}
	cases := []struct {
		name      string
		now       time.Time
		candidate land.ReviewRecord
		want      int
	}{
		{"in-time other bead", in.Add(2 * time.Hour), land.ReviewRecord{BeadID: other, At: inTime, FindingText: []land.ReviewFindingText{restated}}, 1},
		{"record at the same instant", in.Add(2 * time.Hour), land.ReviewRecord{BeadID: other, At: in, FindingText: []land.ReviewFindingText{restated}}, 0},
		{"repeat by the same bead", in.Add(2 * time.Hour), land.ReviewRecord{BeadID: held, At: inTime, FindingText: []land.ReviewFindingText{restated}}, 0},
		{"repeat after 28 days", in.Add(40 * 24 * time.Hour), land.ReviewRecord{BeadID: other, At: in.AddDate(0, 0, landOmStatsRecurrenceFollowDays+1), FindingText: []land.ReviewFindingText{restated}}, 0},
		{"similar finding on another file", in.Add(2 * time.Hour), land.ReviewRecord{BeadID: other, At: inTime, FindingText: []land.ReviewFindingText{elsewhere}}, 0},
		{"restating record after now", in.Add(2 * time.Hour), land.ReviewRecord{BeadID: other, At: in.Add(3 * time.Hour), FindingText: []land.ReviewFindingText{restated}}, 0},
	}
	for _, c := range cases {
		row := recurrence(records(c.candidate), c.now)[0]
		if row.ID != class || row.HeldOut.Findings != 1 || row.HeldOut.Followed != c.want {
			t.Errorf("%s: row = %+v, want %q with one held out and %d followed", c.name, row, class, c.want)
		}
	}
}

// TestLandOmStatsRecurrenceTalliesCounts: two minors on a held-out bead are
// held out and one on a filed bead is filed, while a queue line, a no-bead
// record, a minor of another class and a major count on neither side.
func TestLandOmStatsRecurrenceTalliesCounts(t *testing.T) {
	t.Parallel()
	const held, filed = "gt-rec-0004", "gt-rec-0000"
	class, in := omlessons.HoldOutClasses()[0], omlessons.HoldOutStart().Add(time.Hour)
	if !omlessons.HeldOut(held, class, in) || omlessons.HeldOut(filed, class, in) {
		t.Fatalf("fixture beads %s and %s are not on the sides their names say", held, filed)
	}
	doc := land.ReviewFindingText{Severity: "minor", Title: "the documentation says the flag defaults to true"}
	scope := land.ReviewFindingText{Severity: "minor", Title: "the change reaches outside the scope"}
	major := land.ReviewFindingText{Severity: "major", Title: "the documentation says the flag defaults to true"}
	records := []land.ReviewRecord{
		{BeadID: held, At: in, FindingText: []land.ReviewFindingText{doc, doc, scope}},
		{BeadID: held, At: in, Queue: &land.FollowupQueue{}, FindingText: []land.ReviewFindingText{doc}},
		{At: in, FindingText: []land.ReviewFindingText{doc}},
		{BeadID: filed, At: in, FindingText: []land.ReviewFindingText{doc, major}},
	}
	row := recurrence(records, in)[0]
	if row.ID != class || row.HeldOut.Findings != 2 || row.Filed.Findings != 1 ||
		row.HeldOut.Followed != 0 || row.Filed.Followed != 0 {
		t.Errorf("row = %+v, want %q with 2 held out, 1 filed, none followed", row, class)
	}
}

// TestLandOmStatsRecurrenceClock: a finding recorded after now is not tallied,
// one at now is, and an earlier now counts fewer than a later one, because a
// record the report cannot yet have seen is no evidence (gt-c9sr9p).
func TestLandOmStatsRecurrenceClock(t *testing.T) {
	t.Parallel()
	doc := land.ReviewFindingText{Severity: "minor", Title: "the documentation says the flag defaults to true"}
	now := omlessons.HoldOutStart().Add(48 * time.Hour)
	records := []land.ReviewRecord{
		{BeadID: "gt-rec-0000", At: now.Add(-49 * time.Hour), FindingText: []land.ReviewFindingText{doc}}, // before the window
		{BeadID: "gt-rec-0000", At: now.Add(-time.Hour), FindingText: []land.ReviewFindingText{doc}},      // before now
		{BeadID: "gt-rec-0000", At: now, FindingText: []land.ReviewFindingText{doc}},                      // at now
		{BeadID: "gt-rec-0000", At: now.Add(time.Hour), FindingText: []land.ReviewFindingText{doc}},       // after now
	}
	count := func(at time.Time) int {
		row := recurrence(records, at)[0]
		return row.HeldOut.Findings + row.Filed.Findings
	}
	if got := count(now); got != 2 {
		t.Errorf("at now: %d tallied, want 2 (one before and one at now, none after)", got)
	}
	if got := count(now.Add(-2 * time.Hour)); got != 0 {
		t.Errorf("two hours earlier: %d tallied, want 0", got)
	}
	if earlier, later := count(now.Add(-time.Hour)), count(now.Add(time.Hour)); earlier >= later {
		t.Errorf("earlier now tallied %d, later now %d; want earlier < later", earlier, later)
	}
}

// TestLandOmStatsRecurrenceTalliesWindow: a finding counts from the window's
// first instant up to but not including its end.
func TestLandOmStatsRecurrenceTalliesWindow(t *testing.T) {
	t.Parallel()
	class, start := omlessons.HoldOutClasses()[0], omlessons.HoldOutStart()
	end := start.AddDate(0, 0, omlessons.HoldOutDays)
	doc := land.ReviewFindingText{Severity: "minor", Title: "the documentation says the flag defaults to true"}
	for _, c := range []struct {
		at   time.Time
		want int
	}{{start.Add(-time.Second), 0}, {start, 1}, {end.Add(-time.Second), 1}, {end, 0}} {
		row := recurrence([]land.ReviewRecord{{BeadID: "gt-rec-0000", At: c.at, FindingText: []land.ReviewFindingText{doc}}}, end)[0]
		if got := row.HeldOut.Findings + row.Filed.Findings; got != c.want || row.ID != class {
			t.Errorf("finding at %s: %d counted, want %d", c.at, got, c.want)
		}
	}
}

// landOmStatsRecurrenceOutputs runs the report over a held-out bead and a filed
// bead, each with a minor finding a later record of another bead restates, and
// one more minor finding on the first bead that nothing restates, so the two
// sides differ (the held side two findings and one follow, the filed side one and
// one, or the reverse): a table that swapped the sides' columns would then fail.
// The fixed now is the only clock; the report is returned as text and as JSON.
func landOmStatsRecurrenceOutputs(t *testing.T) (text, raw string) {
	t.Helper()
	in := omlessons.HoldOutStart().Add(time.Hour)
	doc := land.ReviewFindingText{Severity: "minor", File: "internal/cmd/foo.go", Title: "the documentation says the flag defaults to true"}
	alone := land.ReviewFindingText{Severity: "minor", File: "internal/cmd/bar.go", Title: "the documentation says the flag is required"}
	later := land.ReviewFindingText{Severity: "major", File: doc.File, Title: doc.Title}
	var lines []string
	for i, bead := range []string{"gt-rec-0004", "gt-rec-0000"} {
		found := []land.ReviewFindingText{doc}
		if i == 0 {
			found = append(found, alone)
		}
		for _, rec := range []land.ReviewRecord{{BeadID: bead, At: in, FindingText: found}, {BeadID: bead + "-later", At: in.Add(time.Hour), FindingText: []land.ReviewFindingText{later}}} {
			line, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			lines = append(lines, string(line))
		}
	}
	return landOmStatsRecurrenceRun(t, lines, in.Add(time.Hour))
}

// landOmStatsRecurrenceRun runs the report over the record lines at a fixed now,
// through the fakes and never through the command's package-level flags, as text
// and as JSON.
func landOmStatsRecurrenceRun(t *testing.T, lines []string, now time.Time) (text, raw string) {
	t.Helper()
	fake := &landOmStatsFakeFS{files: map[string]string{"/rec": strings.Join(lines, "\n") + "\n", "/log": ""}}
	report := func(asJSON bool) string {
		var out strings.Builder
		if err := landOmStatsRecurrenceReportFrom(fake, omStatsNoTown(t), &out, "", "/rec", "/log", asJSON, now); err != nil {
			t.Fatalf("the report: %v", err)
		}
		return out.String()
	}
	return report(false), report(true)
}

// landOmStatsRecurrenceTableLine is the table row of class id in text.
func landOmStatsRecurrenceTableLine(text, id string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, id+" ") {
			return line
		}
	}
	return ""
}

// TestLandOmStatsRecurrenceJSONCarriesTheRows: --recurrence --json prints the base
// report with a "recurrence" key holding each hold-out class's two sides, here
// one finding and one follow per side on the first class (gt-5hg00y).
func TestLandOmStatsRecurrenceJSONCarriesTheRows(t *testing.T) {
	t.Parallel()
	_, raw := landOmStatsRecurrenceOutputs(t)
	var doc landOmStatsRecurrenceDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decoding the JSON: %v", err)
	}
	first := doc.Recurrence[0]
	one, two := landOmStatsRecurrenceSide{Findings: 1, Followed: 1, Rate: 1, TooSmall: true}, landOmStatsRecurrenceSide{Findings: 2, Followed: 1, Rate: 0.5, TooSmall: true}
	if len(doc.Recurrence) != len(omlessons.HoldOutClasses()) || !((first.HeldOut == one && first.Filed == two) || (first.HeldOut == two && first.Filed == one)) {
		t.Errorf("recurrence rows = %+v, want one per class and, on the first, one side with %+v and the other with %+v", doc.Recurrence, one, two)
	}
}

// TestLandOmStatsRecurrenceReportFails drives every failure the recurrence
// report returns: a bad --since, a town it cannot resolve, unreadable records,
// an unreadable log, a log whose read fails inside AggregateOmStats, and a
// writer that refuses the JSON. Each is the report's error with its own reason,
// and no input failure prints a partial report.
func TestLandOmStatsRecurrenceReportFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	perm := func(path string) error { return &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission} }
	// Open records the paths it was asked for, so each row gets a fake of its own:
	// subtests that run in parallel must not share one.
	empty := func() *landOmStatsFakeFS { return &landOmStatsFakeFS{files: map[string]string{"/rec": "", "/log": ""}} }
	tests := []struct {
		name                string
		fsys                landOmStatsFS
		town                func() (string, error)
		since, records, log string
		json                bool
		out                 io.Writer
		want                string
		errIs               error
	}{
		{"bad --since", empty(), omStatsNoTown(t), "not-a-day", "/rec", "/log", false, nil, "--since", nil},
		{"unresolvable town", empty(), func() (string, error) { return "", boom }, "", "", "", false, nil, "resolving the town:", boom},
		{"unreadable records", &landOmStatsFakeFS{errs: map[string]error{"/rec": perm("/rec")}}, omStatsNoTown(t), "", "/rec", "/log", false, nil, "opening /rec", fs.ErrPermission},
		{"records that do not parse", &landOmStatsFakeFS{files: map[string]string{"/rec": "not json\n", "/log": ""}}, omStatsNoTown(t), "", "/rec", "/log", false, nil, "do not parse", nil},
		{"unreadable log", &landOmStatsFakeFS{files: map[string]string{"/rec": ""}, errs: map[string]error{"/log": perm("/log")}}, omStatsNoTown(t), "", "/rec", "/log", false, nil, "opening /log", fs.ErrPermission},
		{"log read fails", landOmStatsWireFS{}, omStatsNoTown(t), "", "/rec", "/log", false, nil, "reading the daemon log", nil},
		{"JSON unwritable", empty(), omStatsNoTown(t), "", "/rec", "/log", true, landOmStatsFailIO{err: boom}, "writing the report", boom},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out := io.Writer(&strings.Builder{})
			if c.out != nil {
				out = c.out
			}
			err := landOmStatsRecurrenceReportFrom(c.fsys, c.town, out, c.since, c.records, c.log, c.json, time.Time{})
			if err == nil {
				t.Fatalf("%s read as a report", c.name)
			}
			if !strings.Contains(err.Error(), c.want) || (c.errIs != nil && !errors.Is(err, c.errIs)) {
				t.Errorf("%s: error %q, want %q / %v", c.name, err, c.want, c.errIs)
			}
			if b, ok := out.(*strings.Builder); ok && b.Len() != 0 {
				t.Errorf("printed a report after the error: %s", b)
			}
		})
	}
}

// TestLandOmStatsRecurrenceTextAndJSONAgree: each class's table row carries the
// counts and rates its JSON "recurrence" row carries, on a fixture whose two sides
// differ, so a table that swapped the held and filed columns fails (gt-2xv79k).
func TestLandOmStatsRecurrenceTextAndJSONAgree(t *testing.T) {
	t.Parallel()
	text, raw := landOmStatsRecurrenceOutputs(t)
	var doc landOmStatsRecurrenceDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decoding the JSON: %v", err)
	}
	for _, row := range doc.Recurrence {
		want := fmt.Sprintf("%s %d %d %s %d %d %s", row.ID,
			row.HeldOut.Findings, row.HeldOut.Followed, row.HeldOut.rateText(),
			row.Filed.Findings, row.Filed.Followed, row.Filed.rateText())
		line := strings.Join(strings.Fields(landOmStatsRecurrenceTableLine(text, row.ID)), " ")
		if note := strings.HasSuffix(line, "too small to compare"); note != (row.HeldOut.TooSmall || row.Filed.TooSmall) {
			t.Errorf("%s: table note %t, JSON too_small %t/%t", row.ID, note, row.HeldOut.TooSmall, row.Filed.TooSmall)
		}
		if got := strings.TrimSuffix(line, " too small to compare"); got != want {
			t.Errorf("table row %q, want %q", got, want)
		}
	}
}

// landOmStatsRecurrenceBeadsMax bounds the search for beads of each side: the
// hold-out puts about a fifth of beads on the held side, so a few hundred
// candidates fill a side of thirty, and a search that runs out fails the test
// instead of spinning.
const landOmStatsRecurrenceBeadsMax = 10000

// landOmStatsRecurrenceBeads takes beads ("gt-big-0000", ...) until heldN fall on
// the held side and filedN on the filed side, as heldOut reports them, and says
// false when landOmStatsRecurrenceBeadsMax candidates did not fill both sides.
func landOmStatsRecurrenceBeads(heldN, filedN int, heldOut func(bead string) bool) (held, filed []string, ok bool) {
	for i := 0; i < landOmStatsRecurrenceBeadsMax; i++ {
		if len(held) >= heldN && len(filed) >= filedN {
			return held, filed, true
		}
		bead := fmt.Sprintf("gt-big-%04d", i)
		if heldOut(bead) && len(held) < heldN {
			held = append(held, bead)
		} else if !heldOut(bead) && len(filed) < filedN {
			filed = append(filed, bead)
		}
	}
	return held, filed, len(held) >= heldN && len(filed) >= filedN
}

// TestLandOmStatsRecurrenceBeadSearchIsBounded: a hold-out that never answers
// true makes the search stop with the counts it reached, not spin.
func TestLandOmStatsRecurrenceBeadSearchIsBounded(t *testing.T) {
	t.Parallel()
	held, filed, ok := landOmStatsRecurrenceBeads(3, 2, func(string) bool { return false })
	if ok || len(held) != 0 || len(filed) != 2 {
		t.Errorf("search = %d held, %d filed, ok %v; want 0 held, 2 filed and a failed search", len(held), len(filed), ok)
	}
}

// TestLandOmStatsRecurrenceSmallSample: a side with fewer than 30 findings prints
// the too-small note and the JSON carries the flag, while a class with 30 or more
// on both sides prints no note and its flags are false (gt-4tw8kl).
func TestLandOmStatsRecurrenceSmallSample(t *testing.T) {
	t.Parallel()
	small, smallRaw := landOmStatsRecurrenceOutputs(t)
	var doc landOmStatsRecurrenceDoc
	if err := json.Unmarshal([]byte(smallRaw), &doc); err != nil {
		t.Fatalf("decoding the JSON: %v", err)
	}
	first := doc.Recurrence[0]
	if !first.HeldOut.TooSmall || !first.Filed.TooSmall || !strings.HasSuffix(strings.TrimSpace(landOmStatsRecurrenceTableLine(small, first.ID)), "too small to compare") {
		t.Errorf("a side under %d findings: JSON %+v, table %q, want the flags and the note", landOmStatsRecurrenceMinFindings, first, landOmStatsRecurrenceTableLine(small, first.ID))
	}
	// The first class with heldN findings held out and filedN filed: one finding
	// per bead, beads taken until each side holds its count.
	in, class := omlessons.HoldOutStart().Add(time.Hour), omlessons.HoldOutClasses()[0]
	f := land.ReviewFindingText{Severity: "minor", File: "internal/cmd/foo.go", Title: "the documentation says the flag defaults to true"}
	sides := func(heldN, filedN int) (string, landOmStatsRecurrenceClass) {
		held, filed, ok := landOmStatsRecurrenceBeads(heldN, filedN, func(bead string) bool { return omlessons.HeldOut(bead, class, in) })
		if !ok {
			t.Fatalf("%d candidate beads gave %d held and %d filed, want %d and %d", landOmStatsRecurrenceBeadsMax, len(held), len(filed), heldN, filedN)
		}
		var lines []string
		for _, bead := range append(held, filed...) {
			line, err := json.Marshal(land.ReviewRecord{BeadID: bead, At: in, FindingText: []land.ReviewFindingText{f}})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			lines = append(lines, string(line))
		}
		text, raw := landOmStatsRecurrenceRun(t, lines, in.Add(time.Hour))
		var got landOmStatsRecurrenceDoc
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("decoding the JSON: %v", err)
		}
		return landOmStatsRecurrenceTableLine(text, class), got.Recurrence[0]
	}
	threshold := landOmStatsRecurrenceMinFindings
	if line, row := sides(threshold, threshold); row.HeldOut.TooSmall || row.Filed.TooSmall || strings.Contains(line, "too small") {
		t.Errorf("%d findings per side: JSON %+v, table %q, want no flag and no note", threshold, row, line)
	}
	if line, row := sides(threshold, threshold-1); row.HeldOut.TooSmall || !row.Filed.TooSmall || !strings.HasSuffix(strings.TrimSpace(line), "too small to compare") {
		t.Errorf("%d held and %d filed: JSON %+v, table %q, want the note and only the filed flag", threshold, threshold-1, row, line)
	}
}

// TestLandOmStatsRecurrenceZeroFindings: a side with no findings prints "-" for
// its rate and carries rate 0 in the JSON, not a measured "0.0%" (gt-4tw8kl).
func TestLandOmStatsRecurrenceZeroFindings(t *testing.T) {
	t.Parallel()
	text, raw := landOmStatsRecurrenceOutputs(t)
	var doc landOmStatsRecurrenceDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decoding the JSON: %v", err)
	}
	var empty landOmStatsRecurrenceClass
	for _, row := range doc.Recurrence {
		if row.HeldOut.Findings == 0 && row.Filed.Findings == 0 {
			empty = row
			break
		}
	}
	if empty.ID == "" || empty.HeldOut.Rate != 0 || empty.Filed.Rate != 0 {
		t.Fatalf("no class with no findings and a zero rate in %+v", doc.Recurrence)
	}
	if got := strings.Join(strings.Fields(landOmStatsRecurrenceTableLine(text, empty.ID)), " "); !strings.HasPrefix(got, empty.ID+" 0 0 - 0 0 -") {
		t.Errorf("table row %q, want the placeholder \"-\" for both rates", got)
	}
}
