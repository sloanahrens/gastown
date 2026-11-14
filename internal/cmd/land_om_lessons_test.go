package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

var updateLandOmLessonsGolden = flag.Bool("update-land-om-lessons-golden", false, "rewrite testdata/land_om_lessons.golden and .json.golden")

const (
	landOmLessonsGoldenPath     = "land_om_lessons.golden"
	landOmLessonsJSONGoldenPath = "land_om_lessons.json.golden"
)

// landOmLessonsNow is the instant every case runs the report at, so the
// windows fall on fixed days.
var landOmLessonsNow = time.Date(2026, 10, 30, 9, 0, 0, 0, time.Local)

// omLessonsFixtureRecords is the review log every case reads: four verdicts in
// the window, two in the one before, a record with no finding text, and a
// finding no class matches.
func omLessonsFixtureRecords(t *testing.T) []land.ReviewRecord {
	t.Helper()
	at := func(d, hour int) time.Time { return time.Date(2026, 10, d, hour, 0, 0, 0, time.Local) }
	return []land.ReviewRecord{
		{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.4, Verdict: land.VerdictRequestChanges, At: at(25, 10),
			FindingText: []land.ReviewFindingText{{ID: "f-1", Severity: "major", File: "internal/land/note.go", Line: 12, Title: "the refusal branch has no test"}}},
		{BeadID: "gt-a", Rig: "gastown", Attempt: 2, Score: 0.6, Verdict: land.VerdictRequestChanges, At: at(26, 10),
			FindingText: []land.ReviewFindingText{{ID: "f-2", Severity: "major", File: "note_test.go", Line: 40, Title: "a test that passes with the logging call deleted"}}},
		{BeadID: "gt-a", Rig: "gastown", Attempt: 3, Score: 0.9, Verdict: land.VerdictApprove, At: at(27, 10),
			FindingText: []land.ReviewFindingText{{ID: "f-3", Severity: "minor", Title: "the comment carries a stale line number"}}},
		{BeadID: "gt-g", Rig: "gastown", Attempt: 2, Score: 0.5, Verdict: land.VerdictRequestChanges, At: at(28, 10),
			FindingText: []land.ReviewFindingText{
				{ID: "f-4", Severity: "major", Title: "the gate reads a missing file as clean"},
				{ID: "f-5", Severity: "minor", Title: "a finding from a class not yet seeded"},
			}},
		{BeadID: "gt-c", Rig: "longeye", Attempt: 1, Score: 0.9, Verdict: land.VerdictApprove, At: at(19, 10)},
		{BeadID: "gt-d", Rig: "longeye", Attempt: 1, Score: 0.5, Verdict: land.VerdictRequestChanges, At: at(20, 10),
			FindingText: []land.ReviewFindingText{{ID: "f-6", Severity: "minor", Title: "the test sleeps waiting for the daemon"}}},
	}
}

// writeLandOmLessonsRecords writes the fixture as the review log's own lines
// and returns its path.
func writeLandOmLessonsRecords(t *testing.T, dir string, records []land.ReviewRecord) string {
	t.Helper()
	var lines []string
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		lines = append(lines, string(line))
	}
	path := filepath.Join(dir, land.ReviewsFileName)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing the records: %v", err)
	}
	return path
}

func landOmLessonsFixture(t *testing.T) (recordsPath, dir string) {
	t.Helper()
	dir = t.TempDir()
	return writeLandOmLessonsRecords(t, dir, omLessonsFixtureRecords(t)), dir
}

// townThatFails is the town resolution a run must report rather than read as
// an empty report.
func townThatFails() (string, error) {
	return "", errors.New("not in a town")
}

// TestLandOmLessonsGolden pins the table: one row per class with the window
// beside the one before it, then the titles no class matched.
func TestLandOmLessonsGolden(t *testing.T) {
	t.Parallel()
	path, _ := landOmLessonsFixture(t)
	var out bytes.Buffer
	if err := landOmLessonsReport(&out, landOmLessonsNow, "", omlessons.DefaultDays, path, false); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", landOmLessonsGoldenPath)
	if *updateLandOmLessonsGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-land-om-lessons-golden):\n%s\nwant:\n%s", golden, out.String(), want)
	}
}

// TestLandOmLessonsJSONGolden pins the report the table renders, so a reader
// taking figures from --json gets the same ones.
func TestLandOmLessonsJSONGolden(t *testing.T) {
	t.Parallel()
	path, _ := landOmLessonsFixture(t)
	var out bytes.Buffer
	if err := landOmLessonsReport(&out, landOmLessonsNow, "", omlessons.DefaultDays, path, true); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", landOmLessonsJSONGoldenPath)
	if *updateLandOmLessonsGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-land-om-lessons-golden)", golden)
	}
}

// TestLandOmLessonsWritesNoFile: the command is read-only, so a run leaves
// the records file as it was and writes nothing beside it.
func TestLandOmLessonsWritesNoFile(t *testing.T) {
	t.Parallel()
	path, dir := landOmLessonsFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := landOmLessonsReport(&out, landOmLessonsNow, "", omlessons.DefaultDays, path, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the records file changed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != land.ReviewsFileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want only %s", names, land.ReviewsFileName)
	}
}

// TestLandOmLessonsNamedRecordsMustExist: a typo in --records is an error,
// not an empty report.
func TestLandOmLessonsNamedRecordsMustExist(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-file.jsonl")
	var out bytes.Buffer
	err := landOmLessonsReport(&out, landOmLessonsNow, "", omlessons.DefaultDays, missing, false)
	if err == nil {
		t.Fatal("no error for a named path that does not exist")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to say the path does not exist", err)
	}
}

// TestLandOmLessonsReadsTheTownDefault: with no --records, the report reads
// the town's own review log, which a town keeps under .runtime/landings.
func TestLandOmLessonsReadsTheTownDefault(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	landings := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(landings, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLandOmLessonsRecords(t, landings, omLessonsFixtureRecords(t))
	var out bytes.Buffer
	if err := landOmLessonsReportFrom(func() (string, error) { return town, nil },
		&out, landOmLessonsNow, "", omlessons.DefaultDays, "", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "4 verdict(s) in the window") {
		t.Errorf("output did not read the town's log:\n%s", out.String())
	}
}

// TestLandOmLessonsToleratesAbsentTownDefault: a town that has not landed yet
// has no review log, and that is an empty report rather than a failure.
func TestLandOmLessonsToleratesAbsentTownDefault(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out bytes.Buffer
	if err := landOmLessonsReportFrom(func() (string, error) { return town, nil },
		&out, landOmLessonsNow, "", omlessons.DefaultDays, "", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 verdict(s) in the window, 0 in the prior") {
		t.Errorf("output did not report an empty window:\n%s", out.String())
	}
}

// TestLandOmLessonsReportsATownItCannotResolve: the town is read when no
// --records names a file, so a resolution that fails must fail the run.
func TestLandOmLessonsReportsATownItCannotResolve(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := landOmLessonsReportFrom(townThatFails, &out, landOmLessonsNow, "", omlessons.DefaultDays, "", false)
	if err == nil {
		t.Fatal("no error for a town that cannot be resolved")
	}
	if !strings.Contains(err.Error(), "resolving the town") {
		t.Errorf("error = %v, want it to name the resolution", err)
	}
}

// TestLandOmLessonsNamesAnUnreadableRecordsFile: a path that exists but will
// not read is an error naming it, not an empty report.
func TestLandOmLessonsNamesAnUnreadableRecordsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var out bytes.Buffer
	err := landOmLessonsReport(&out, landOmLessonsNow, "", omlessons.DefaultDays, dir, false)
	if err == nil {
		t.Fatal("no error for a directory read as the records file")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %v, want it to name %s", err, dir)
	}
}

// TestLandOmLessonsNamesAPathItCannotAsk: a named path that a failed stat
// leaves unreadable is an error naming it, not a report.
func TestLandOmLessonsNamesAPathItCannotAsk(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, land.ReviewsFileName)
	var out bytes.Buffer
	err := landOmLessonsReport(&out, landOmLessonsNow, "", omlessons.DefaultDays, path, false)
	if err == nil {
		t.Fatal("no error for a path below a file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name %s", err, path)
	}
}

// TestLandOmLessonsReportsAWriteFailure: --json writes to the command's
// output, so a refused write is the run's error rather than a silent report.
func TestLandOmLessonsReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	path, _ := landOmLessonsFixture(t)
	if err := landOmLessonsReport(failingWriter{}, landOmLessonsNow, "", omlessons.DefaultDays, path, true); err == nil {
		t.Fatal("no error for a refused write")
	}
}

// TestLandOmLessonsReportsATableWriteFailure: the table is the default output,
// so a refused write is the run's error there too, not a silent report.
func TestLandOmLessonsReportsATableWriteFailure(t *testing.T) {
	t.Parallel()
	path, _ := landOmLessonsFixture(t)
	if err := landOmLessonsReport(failingWriter{}, landOmLessonsNow, "", omlessons.DefaultDays, path, false); err == nil {
		t.Fatal("no error for a refused table write")
	}
}

// TestLandOmLessonsEscapesAnUnclassifiedTitle: a title is om's text, so an
// escape sequence in it prints as visible text, not as a terminal command.
func TestLandOmLessonsEscapesAnUnclassifiedTitle(t *testing.T) {
	t.Parallel()
	path := writeLandOmLessonsRecords(t, t.TempDir(), []land.ReviewRecord{{
		BeadID: "gt-x", Rig: "gastown", Attempt: 1, Score: 0.4,
		Verdict: land.VerdictRequestChanges, At: time.Date(2026, 10, 28, 10, 0, 0, 0, time.Local),
		FindingText: []land.ReviewFindingText{{ID: "f-7", Severity: "minor",
			Title: "\x1b[2J\x1b]0;owned\a a finding from a class not yet seeded"}},
	}})
	var out bytes.Buffer
	if err := landOmLessonsReport(&out, landOmLessonsNow, "", omlessons.DefaultDays, path, false); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\a") {
		t.Errorf("output carries a raw control byte:\n%q", out.String())
	}
	if !strings.Contains(out.String(), `\x1b[2J\x1b]0;owned\x07 a finding from a class not yet seeded`) {
		t.Errorf("output does not show the escaped title:\n%s", out.String())
	}
}

// TestLandOmLessonsRejectsAnEmptyWindow: --days names the window's length, so
// a length under one is an error.
func TestLandOmLessonsRejectsAnEmptyWindow(t *testing.T) {
	t.Parallel()
	path, _ := landOmLessonsFixture(t)
	for _, days := range []int{0, -1} {
		var out bytes.Buffer
		if err := landOmLessonsReport(&out, landOmLessonsNow, "", days, path, false); err == nil {
			t.Errorf("--days %d: no error", days)
		}
	}
}

// TestLandOmLessonsRejectsABadSince: --since is a calendar day, and a value
// that is not one is an error rather than a window in the distant past.
func TestLandOmLessonsRejectsABadSince(t *testing.T) {
	t.Parallel()
	path, _ := landOmLessonsFixture(t)
	var out bytes.Buffer
	err := landOmLessonsReport(&out, landOmLessonsNow, "2026-13-01", omlessons.DefaultDays, path, false)
	if err == nil {
		t.Fatal("no error for --since 2026-13-01")
	}
	if !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Errorf("error = %v, want it to name the accepted form", err)
	}
}

// landOmLessonsFixtureDirectives is a directive set with rules 1, 2 and 7 in
// polecat.md and none in the shared file: rule 3 is absent, so a class under it
// reads as a gap naming the missing rule.
var landOmLessonsFixtureDirectives = filepath.Join("testdata", "om_lessons_directives")

// omLessonsProposeRecords is the review log the proposal cases read, all in the
// window of 2026-10-24 to 2026-10-30: six fail-open findings (rule 1, which the
// fixture directive has), five wall-clock findings (no rule), five
// refusal-branch findings (rule 3, which the fixture lacks), two secret
// findings (too few), one fail-open finding in the prior window, and one title
// no class matches.
func omLessonsProposeRecords() []land.ReviewRecord {
	at := func(d int) time.Time { return time.Date(2026, 10, d, 10, 0, 0, 0, time.Local) }
	rec := func(bead string, attempt, d int, sev, title string) land.ReviewRecord {
		return land.ReviewRecord{BeadID: bead, Rig: "gastown", Attempt: attempt, Score: 0.5,
			Verdict: land.VerdictRequestChanges, At: at(d),
			FindingText: []land.ReviewFindingText{{ID: "f", Severity: sev, Title: title}}}
	}
	return []land.ReviewRecord{
		rec("gt-a", 1, 24, "major", "the gate reads a failed read as clean"),
		rec("gt-a", 2, 25, "major", "the gate reads a failed read as clean"),
		rec("gt-b", 1, 25, "critical", "error reads as success on an empty status"),
		rec("gt-c", 1, 26, "major", "treated as absent when the file is missing"),
		rec("gt-c", 2, 27, "minor", "fail-open on unreadable routes"),
		rec("gt-d", 1, 27, "major", "fail open when the lock is missing"),
		rec("gt-e", 1, 24, "minor", "the test sleeps waiting for the daemon"),
		rec("gt-f", 1, 25, "minor", "a busy-wait on the wall clock"),
		rec("gt-g", 1, 26, "minor", "polls until the daemon answers"),
		rec("gt-h", 1, 27, "minor", "a sleep before the check"),
		rec("gt-i", 1, 28, "minor", "a wait of two seconds"),
		rec("gt-j", 1, 24, "major", "the refusal branch has no test"),
		rec("gt-k", 1, 25, "major", "the failing branch has no assertion"),
		rec("gt-l", 1, 26, "major", "an untested error path"),
		rec("gt-m", 1, 27, "major", "the error path has no test"),
		rec("gt-n", 1, 28, "major", "lacks a test for the refusal"),
		rec("gt-o", 1, 25, "major", "the token is echoed in the error"),
		rec("gt-p", 1, 26, "major", "a password reaches the log"),
		rec("gt-q", 1, 19, "major", "reads as clean on a bad input"),
		rec("gt-r", 1, 29, "minor", "a finding from a class not yet seeded"),
	}
}

// TestLandOmLessonsProposeGolden pins the drafts: a class with enough findings
// gets one, with its coverage, counts and examples; a class under the threshold
// is a too-few line with no draft.
func TestLandOmLessonsProposeGolden(t *testing.T) {
	t.Parallel()
	path := writeLandOmLessonsRecords(t, t.TempDir(), omLessonsProposeRecords())
	var out bytes.Buffer
	if err := landOmLessonsProposeFrom(townThatFails, &out, landOmLessonsNow, "", omlessons.DefaultDays,
		path, landOmLessonsFixtureDirectives); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", landOmLessonsProposeGoldenPath)
	if *updateLandOmLessonsGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-land-om-lessons-golden):\n%s\nwant:\n%s", golden, out.String(), want)
	}
}

const landOmLessonsProposeGoldenPath = "land_om_lessons_propose.golden"

// TestLandOmLessonsProposeRefusesUnreadableRules: the coverage check reads the
// directive files, so one that cannot be read, or a directive set with no
// numbered rule at all, is an error. Without that, every class reads as a gap.
func TestLandOmLessonsProposeRefusesUnreadableRules(t *testing.T) {
	t.Parallel()
	path := writeLandOmLessonsRecords(t, t.TempDir(), omLessonsProposeRecords())
	cases := []struct {
		name    string
		dir     func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "a directory that does not exist",
			dir:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			wantErr: "reading the directive",
		},
		{
			name: "polecat.md missing",
			dir: func(t *testing.T) string {
				return writeLandOmLessonsDirectives(t, map[string]string{"_common.md": "## 1. shared rule\n"})
			},
			wantErr: "reading the directive",
		},
		{
			name: "no numbered rule in either file",
			dir: func(t *testing.T) string {
				return writeLandOmLessonsDirectives(t, map[string]string{
					"polecat.md": "## Tests\n\n## 1 without a dot is not a rule\n",
					"_common.md": "## Changelog\n",
				})
			},
			wantErr: "holds no numbered rules",
		},
		{
			name: "a rule only in the shared file counts",
			dir: func(t *testing.T) string {
				return writeLandOmLessonsDirectives(t, map[string]string{
					"polecat.md": "## Tests\n",
					"_common.md": "## 1. shared rule\n",
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := landOmLessonsProposeFrom(townThatFails, &out, landOmLessonsNow, "", omlessons.DefaultDays, path, tc.dir(t))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// writeLandOmLessonsDirectives writes the named directive files into a fresh
// directory and returns it.
func writeLandOmLessonsDirectives(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestLandOmLessonsProposeRunE drives the cobra command, so the --propose
// dispatch and its refusal of --json are wired to the flags. It sets the
// command's flags, which are package state, so it does not run in parallel and
// restores the defaults when it is done.
//
//testpolicy:allow parallel — sets the om-lessons command's package-level flags, which a parallel test would share
func TestLandOmLessonsProposeRunE(t *testing.T) {
	path := writeLandOmLessonsRecords(t, t.TempDir(), omLessonsProposeRecords())
	setFlag := func(name, value string) {
		t.Helper()
		if err := landOmLessonsCmd.Flags().Set(name, value); err != nil {
			t.Fatalf("setting --%s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		setFlag("propose", "false")
		setFlag("json", "false")
		setFlag("since", "")
		setFlag("records", "")
		setFlag("directives", "directives")
		landOmLessonsCmd.SetOut(nil)
	})
	setFlag("since", "2026-10-24")
	setFlag("records", path)
	setFlag("directives", landOmLessonsFixtureDirectives)

	var out bytes.Buffer
	landOmLessonsCmd.SetOut(&out)
	setFlag("propose", "true")
	if err := landOmLessonsCmd.RunE(landOmLessonsCmd, nil); err != nil {
		t.Fatalf("RunE with --propose: %v", err)
	}
	if !strings.Contains(out.String(), "om lessons proposals: 2026-10-24 to 2026-10-30") {
		t.Errorf("--propose did not print the proposals:\n%s", out.String())
	}

	setFlag("json", "true")
	if err := landOmLessonsCmd.RunE(landOmLessonsCmd, nil); err == nil {
		t.Error("--propose with --json: no error")
	}
}
