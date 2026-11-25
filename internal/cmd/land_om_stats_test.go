package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/land"
)

// omStatsFixtureLines renders the two sources gt land om-stats reads: three
// verdicts on two days, and a daemon-log day before the first record.
func omStatsFixtureLines(t *testing.T) (records, log string) {
	t.Helper()
	day := func(d, hour int) time.Time { return time.Date(2026, 10, d, hour, 0, 0, 0, time.Local) }
	recs := []land.ReviewRecord{
		{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: land.VerdictRequestChanges, Agent: "opus", Findings: 2, Bar: 0.8, At: day(16, 10)},
		{BeadID: "gt-a", Rig: "gastown", Attempt: 2, Score: 0.90, Verdict: land.VerdictApprove, Agent: "opus", Bar: 0.8, At: day(16, 11)},
		{BeadID: "gt-b", Rig: "longeye", Attempt: 1, Score: 0.85, Verdict: land.VerdictApprove, Agent: "sonnet", Bar: 0.8, At: day(17, 10)},
	}
	var lines []string
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		lines = append(lines, string(line))
	}
	log = strings.Join([]string{
		"2026/10/14 23:10:00 landing_worker: [land] gt-old: rejected (review): om requested changes (score 0.55, 3 finding(s))",
		"2026/10/14 23:40:00 landing_worker: [land] gt-old2: landed abc1234 on origin/main (patch-id def5678)",
	}, "\n") + "\n"
	return strings.Join(lines, "\n") + "\n", log
}

// omStatsFixture writes the fixture into a fresh temporary directory and
// returns the two paths, for the tests that drive the real filesystem.
func omStatsFixture(t *testing.T) (recordsPath, logPath string) {
	t.Helper()
	return writeOmStatsFixture(t, t.TempDir())
}

// writeOmStatsFixture writes the fixture's two files into dir.
func writeOmStatsFixture(t *testing.T, dir string) (recordsPath, logPath string) {
	t.Helper()
	records, log := omStatsFixtureLines(t)
	recordsPath = filepath.Join(dir, land.ReviewsFileName)
	if err := os.WriteFile(recordsPath, []byte(records), 0o600); err != nil {
		t.Fatalf("writing the records: %v", err)
	}
	logPath = filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
		t.Fatalf("writing the log: %v", err)
	}
	return recordsPath, logPath
}

// omStatsTree snapshots every path under dir: for a directory its kind, for a
// file its size, modification time and bytes. Adding, removing or rewriting a
// file changes the snapshot; reading leaves it alone.
func omStatsTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = fmt.Sprintf("%d %d %s", info.Size(), info.ModTime().UnixNano(), data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", dir, err)
	}
	return tree
}

// landOmStatsFakeFS is the in-memory filesystem the tests hand the command. It
// exposes Open and nothing else, so the command has no way to write; opened
// records every path the command reached, and errs names the paths whose open
// fails for a reason other than a missing file.
type landOmStatsFakeFS struct {
	files  map[string]string
	errs   map[string]error
	opened []string
}

func (f *landOmStatsFakeFS) Open(name string) (io.ReadCloser, error) {
	f.opened = append(f.opened, name)
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	data, ok := f.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

// omStatsNoTown fails the test if the report resolves the town, which it must
// not do when the operator named both sources.
func omStatsNoTown(t *testing.T) func() (string, error) {
	return func() (string, error) {
		t.Errorf("the report resolved the town with both sources named")
		return "", nil
	}
}

// runLandOmStatsOutput runs the command body with the flags the operator would
// pass, and returns what it printed.
func runLandOmStatsOutput(t *testing.T, recordsPath, logPath, since string, asJSON bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := landOmStatsReport(&out, since, recordsPath, logPath, asJSON, false); err != nil {
		t.Fatalf("landOmStatsReport: %v", err)
	}
	return out.String()
}

// TestLandOmStatsTextAndJSONAgree is gt-r05qz's command acceptance: the table
// carries the same days and figures the JSON does, and a fallback day is marked
// in both.
func TestLandOmStatsTextAndJSONAgree(t *testing.T) {
	t.Parallel()
	recordsPath, logPath := omStatsFixture(t)

	text := runLandOmStatsOutput(t, recordsPath, logPath, "", false)
	for _, want := range []string{
		"om landings, all days",
		"2026-10-14*",
		"2026-10-16",
		"2026-10-17",
		"* filled from the daemon log: no review record for the day",
		"Rig",
		"Agent",
		"Rounds per bead: 1 took one, 1 two, 0 three or more",
		"Beads rejected most",
		"gt-a",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("table does not carry %q:\n%s", want, text)
		}
	}

	raw := runLandOmStatsOutput(t, recordsPath, logPath, "", true)
	var stats land.OmStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatalf("--json output does not decode: %v\n%s", err, raw)
	}
	if len(stats.Days) != 3 {
		t.Fatalf("days = %+v, want three", stats.Days)
	}
	want := []land.OmStatsDay{
		{Day: "2026-10-14", Approvals: 1, Rejections: 1, Rate: 0.5, MeanFindings: 3, MeanScore: 0.55, Fallback: true},
		{Day: "2026-10-16", Approvals: 1, Rejections: 1, Rate: 0.5, MeanFindings: 2, MeanScore: 0.40},
		{Day: "2026-10-17", Approvals: 1, Rejections: 0, Rate: 0},
	}
	for i, day := range want {
		got := stats.Days[i]
		if got.Day != day.Day || got.Approvals != day.Approvals || got.Rejections != day.Rejections || got.Fallback != day.Fallback {
			t.Errorf("day %d = %+v, want %+v", i, got, day)
		}
	}
	if len(stats.Rigs) != 2 || stats.Rigs[0].Name != "gastown" || stats.Rigs[1].Name != "longeye" {
		t.Errorf("rigs = %+v, want gastown then longeye", stats.Rigs)
	}
	if len(stats.Agents) != 2 || stats.Agents[0].Name != "opus" || stats.Agents[1].Name != "sonnet" {
		t.Errorf("agents = %+v, want opus then sonnet", stats.Agents)
	}
	if stats.Rounds != (land.OmStatsRounds{One: 1, Two: 1}) {
		t.Errorf("rounds = %+v, want one bead at two rounds", stats.Rounds)
	}
	if len(stats.TopBeads) != 1 || stats.TopBeads[0].Bead != "gt-a" || stats.TopBeads[0].Rejections != 1 {
		t.Errorf("top beads = %+v, want gt-a alone", stats.TopBeads)
	}

	// The same figures the JSON carries are the ones the table shows: the row
	// that opens with the day holds its approvals, rejections and rate.
	for _, day := range stats.Days {
		row := ""
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, day.Day) {
				row = line
				break
			}
		}
		if row == "" {
			t.Errorf("table lacks a row for %s", day.Day)
			continue
		}
		fields := strings.Fields(row)
		if len(fields) < 4 || fields[1] != strconv.Itoa(day.Approvals) || fields[2] != strconv.Itoa(day.Rejections) ||
			fields[3] != landOmStatsPercent(day.Rate) {
			t.Errorf("table row %q does not carry the JSON's %d approvals, %d rejections at %s",
				row, day.Approvals, day.Rejections, landOmStatsPercent(day.Rate))
		}
	}
}

// TestLandOmStatsSinceFiltersBothForms pins --since to the day tables and the
// JSON alike: nothing before the day is reported, including a fallback day.
func TestLandOmStatsSinceFiltersBothForms(t *testing.T) {
	t.Parallel()
	recordsPath, logPath := omStatsFixture(t)

	text := runLandOmStatsOutput(t, recordsPath, logPath, "2026-10-17", false)
	if !strings.Contains(text, "om landings since 2026-10-17") {
		t.Errorf("table header does not name the range:\n%s", text)
	}
	for _, banned := range []string{"2026-10-14", "2026-10-16", "* filled from the daemon log"} {
		if strings.Contains(text, banned) {
			t.Errorf("table carries %q before --since:\n%s", banned, text)
		}
	}

	raw := runLandOmStatsOutput(t, recordsPath, logPath, "2026-10-17", true)
	var stats land.OmStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatalf("--json output does not decode: %v", err)
	}
	if len(stats.Days) != 1 || stats.Days[0].Day != "2026-10-17" {
		t.Errorf("days = %+v, want only 2026-10-17", stats.Days)
	}
	if len(stats.TopBeads) != 0 {
		t.Errorf("top beads = %+v, want none after --since", stats.TopBeads)
	}
}

// TestLandOmStatsWritesNothing: the command is a read. Both forms run against
// a real temporary directory, whose tree is snapshotted before and after, so a
// write through os — even one that leaves the bytes the same — changes a size,
// a modification time or the entry list and fails the test. The in-memory fake
// runs beside it and pins the read: the command opens its two sources and
// nothing else.
func TestLandOmStatsWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	recordsPath, logPath := writeOmStatsFixture(t, dir)
	before := omStatsTree(t, dir)

	for _, tc := range []struct {
		since  string
		asJSON bool
	}{{"", false}, {"2026-10-16", true}} {
		var out bytes.Buffer
		if err := landOmStatsReport(&out, tc.since, recordsPath, logPath, tc.asJSON, false); err != nil {
			t.Fatalf("landOmStatsReport: %v", err)
		}
	}

	// The town defaults are the other paths a write could reach: point them at
	// the same directory through the fake, so an os-level write there lands in
	// the snapshot too.
	records, log := omStatsFixtureLines(t)
	townRecords := land.TownReviewsFile(dir).Path
	townLog := filepath.Join(dir, "daemon", daemonlog.FileName)
	fake := &landOmStatsFakeFS{files: map[string]string{
		townRecords: records,
		townLog:     log,
	}}
	var out bytes.Buffer
	if err := landOmStatsReportFrom(fake, func() (string, error) { return dir, nil }, &out, "", "", "", false, false); err != nil {
		t.Fatalf("landOmStatsReportFrom: %v", err)
	}

	after := omStatsTree(t, dir)
	if len(after) != len(before) {
		t.Fatalf("tree entries %d -> %d: the command wrote a path", len(before), len(after))
	}
	for name, want := range before {
		if got, ok := after[name]; !ok {
			t.Errorf("%s disappeared: the command wrote the tree", name)
		} else if got != want {
			t.Errorf("%s changed: %q -> %q", name, want, got)
		}
	}
	for _, name := range fake.opened {
		if name != townRecords && name != townLog {
			t.Errorf("the command opened %q, which is not one of its sources", name)
		}
	}
}

// TestLandOmStatsRejectsABadSince keeps a mistyped day from silently reporting
// nothing.
func TestLandOmStatsRejectsABadSince(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"2026-13-01", "16/10/2026", "yesterday"} {
		if _, err := parseLandOmStatsSince(value); err == nil {
			t.Errorf("parseLandOmStatsSince(%q) = nil error, want a refusal", value)
		}
	}
	if at, err := parseLandOmStatsSince(" 2026-10-16 "); err != nil || at.IsZero() {
		t.Errorf("parseLandOmStatsSince trimmed day = %v, %v", at, err)
	}
	if at, err := parseLandOmStatsSince(""); err != nil || !at.IsZero() {
		t.Errorf("parseLandOmStatsSince(empty) = %v, %v; want the zero time", at, err)
	}
}

// TestLandOmStatsNamedSourceMustExist keeps a mistyped --records or --log from
// reading as a clean, empty report: the run fails and names the path.
func TestLandOmStatsNamedSourceMustExist(t *testing.T) {
	t.Parallel()
	records, log := omStatsFixtureLines(t)
	fake := &landOmStatsFakeFS{files: map[string]string{
		"/town/reviews.jsonl": records,
		"/town/daemon.log":    log,
	}}
	for _, tc := range []struct{ name, recordsPath, logPath, missing string }{
		{"records", "/town/no-reviews.jsonl", "/town/daemon.log", "/town/no-reviews.jsonl"},
		{"log", "/town/reviews.jsonl", "/town/no-daemon.log", "/town/no-daemon.log"},
	} {
		var out bytes.Buffer
		err := landOmStatsReportFrom(fake, omStatsNoTown(t), &out, "", tc.recordsPath, tc.logPath, false, false)
		if err == nil {
			t.Errorf("%s: missing explicit %s read as a report:\n%s", tc.name, tc.name, out.String())
			continue
		}
		if !strings.Contains(err.Error(), tc.missing) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.missing)
		}
		if out.Len() != 0 {
			t.Errorf("%s: printed a report after the error:\n%s", tc.name, out.String())
		}
	}
}

// TestLandOmStatsFailsWhenTheTownCannotBeResolved: with a source left to the
// town default, a resolver failure is the report's error, named with its cause,
// and nothing is printed — not an empty report (gt-r05qz).
func TestLandOmStatsFailsWhenTheTownCannotBeResolved(t *testing.T) {
	t.Parallel()
	boom := errors.New("no town above the working directory")
	findTown := func() (string, error) { return "", boom }

	var out bytes.Buffer
	err := landOmStatsReportFrom(&landOmStatsFakeFS{files: map[string]string{}}, findTown, &out, "", "", "", false, false)
	if err == nil {
		t.Fatalf("a failed town resolution read as a report:\n%s", out.String())
	}
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "resolving the town:") {
		t.Errorf("error %q does not carry the resolution failure %q", err, boom)
	}
	if out.Len() != 0 {
		t.Errorf("printed a report after the error:\n%s", out.String())
	}
}

// TestLandOmStatsSurfacesAnOpenError: a source whose open fails for a reason
// other than being missing is the report's error, named with its path and its
// cause, not an empty report (gt-r05qz).
func TestLandOmStatsSurfacesAnOpenError(t *testing.T) {
	t.Parallel()
	records, log := omStatsFixtureLines(t)
	const recordsPath, logPath = "/town/reviews.jsonl", "/town/daemon.log"
	permission := func(path string) error {
		return &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		errs  map[string]error
		want  string
	}{
		{"records", map[string]string{logPath: log}, map[string]error{recordsPath: permission(recordsPath)}, recordsPath},
		{"log", map[string]string{recordsPath: records}, map[string]error{logPath: permission(logPath)}, logPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := landOmStatsReportFrom(&landOmStatsFakeFS{files: tc.files, errs: tc.errs}, omStatsNoTown(t),
				&out, "", recordsPath, logPath, false, false)
			if err == nil {
				t.Fatalf("an open that failed with %v read as a report:\n%s", fs.ErrPermission, out.String())
			}
			if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "opening "+tc.want) {
				t.Errorf("error %q does not carry the open failure of %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("printed a report after the error:\n%s", out.String())
			}
		})
	}
}

// TestLandOmStatsResolvesTheTownFromCwd drives the production resolver: with
// both flags empty, the report finds the town by walking up from its working
// directory and opens the town's two defaults.
//
//testpolicy:allow parallel — reads the working directory, which another parallel test must not change mid-run
func TestLandOmStatsResolvesTheTownFromCwd(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, log := omStatsFixtureLines(t)
	recordsPath := land.TownReviewsFile(town).Path
	if err := os.MkdirAll(filepath.Dir(recordsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordsPath, []byte(records), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(town, "daemon", daemonlog.FileName)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — the working directory is the input under test: the command finds the town by walking up from it
	t.Chdir(town)

	var out bytes.Buffer
	if err := landOmStatsReport(&out, "", "", "", false, false); err != nil {
		t.Fatalf("landOmStatsReport with empty flags: %v", err)
	}
	for _, want := range []string{"2026-10-14*", "2026-10-16", "2026-10-17"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the town's defaults did not reach the report: %q missing from\n%s", want, out.String())
		}
	}
}

// TestLandOmStatsReadsAndToleratesTheTownDefaults: empty flags resolve to the
// town's two files. A town that has not landed yet has neither, and still
// reports an empty range rather than failing.
func TestLandOmStatsReadsAndToleratesTheTownDefaults(t *testing.T) {
	t.Parallel()
	town := "/town"
	findTown := func() (string, error) { return town, nil }
	report := func(fake *landOmStatsFakeFS) string {
		t.Helper()
		var out bytes.Buffer
		if err := landOmStatsReportFrom(fake, findTown, &out, "", "", "", false, false); err != nil {
			t.Fatalf("landOmStatsReportFrom: %v", err)
		}
		return out.String()
	}

	if got := report(&landOmStatsFakeFS{files: map[string]string{}}); !strings.Contains(got, "no om verdicts in range") {
		t.Errorf("a town without either default file printed %q, want an empty report", got)
	}

	records, log := omStatsFixtureLines(t)
	fake := &landOmStatsFakeFS{files: map[string]string{
		land.TownReviewsFile(town).Path:                   records,
		filepath.Join(town, "daemon", daemonlog.FileName): log,
	}}
	got := report(fake)
	for _, want := range []string{"2026-10-14*", "2026-10-16", "2026-10-17"} {
		if !strings.Contains(got, want) {
			t.Errorf("defaults did not reach the town's files: %q missing from\n%s", want, got)
		}
	}
}

// omStatsAuthorFixtureLines renders a verdict from a polecat's branch and one
// from a crew bird's, so the Author table has a row of each kind; the log is
// empty, so no fallback day enters the report.
func omStatsAuthorFixtureLines(t *testing.T) (records, log string) {
	t.Helper()
	day := func(hour int) time.Time { return time.Date(2026, 10, 16, hour, 0, 0, 0, time.Local) }
	recs := []land.ReviewRecord{
		{BeadID: "gt-p1", Rig: "gastown", Attempt: 1, Score: 0.90, Verdict: land.VerdictApprove,
			Agent: "opus", Branch: "polecat/dag/gt-p1", AuthorKind: authorkind.Polecat, At: day(10)},
		{BeadID: "gt-p2", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: land.VerdictRequestChanges,
			Agent: "opus", Branch: "polecat/dag/gt-p2", AuthorKind: authorkind.Polecat, Findings: 2, At: day(11)},
		{BeadID: "gt-c1", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: land.VerdictRequestChanges,
			Agent: "sonnet", Branch: "crew/bird/gt-c1", AuthorKind: authorkind.Crew, Findings: 1, At: day(12)},
	}
	var lines []string
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		lines = append(lines, string(line))
	}
	return strings.Join(lines, "\n") + "\n", ""
}

// omStatsTableStart returns the offset of the line whose first cell is title,
// or -1. A row or a sentence that names the title later does not match.
func omStatsTableStart(text, title string) int {
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(line, title) && len(line) > len(title) && (line[len(title)] == ' ' || line[len(title)] == '\t') {
			return offset
		}
		offset += len(line)
	}
	return -1
}

// omStatsTableRow returns the fields of the row named name in the table titled
// title, or nil when the table or the row is missing. The row search ends at
// the table's first blank line, so a row of a later table cannot match.
func omStatsTableRow(text, title, name string) []string {
	start := omStatsTableStart(text, title)
	if start < 0 {
		return nil
	}
	for _, line := range strings.Split(text[start:], "\n")[1:] {
		if strings.TrimSpace(line) == "" {
			return nil
		}
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return fields
		}
	}
	return nil
}

// TestLandOmStatsAuthorTable is gt-onszp9's command acceptance: the text form
// prints authority counts per author kind — polecat against crew — in the
// Agent table's layout and after it, and --json carries the same rows under
// "authors". The order row is what a mutation that prints Author first turns
// red.
func TestLandOmStatsAuthorTable(t *testing.T) {
	t.Parallel()
	records, log := omStatsAuthorFixtureLines(t)
	report := func(t *testing.T, asJSON bool) string {
		t.Helper()
		fake := &landOmStatsFakeFS{files: map[string]string{
			"/town/reviews.jsonl": records,
			"/town/daemon.log":    log,
		}}
		var out bytes.Buffer
		if err := landOmStatsReportFrom(fake, omStatsNoTown(t), &out, "", "/town/reviews.jsonl", "/town/daemon.log", asJSON, false); err != nil {
			t.Fatalf("landOmStatsReportFrom: %v", err)
		}
		return out.String()
	}

	t.Run("the Author table follows the Agent table", func(t *testing.T) {
		t.Parallel()
		text := report(t, false)
		agent, author := omStatsTableStart(text, "Agent"), omStatsTableStart(text, "Author")
		if agent < 0 || author < 0 {
			t.Fatalf("Agent table at %d, Author table at %d, want both:\n%s", agent, author, text)
		}
		if author < agent {
			t.Errorf("the Author table at %d precedes the Agent table at %d:\n%s", author, agent, text)
		}
	})

	t.Run("each author kind's counts", func(t *testing.T) {
		t.Parallel()
		text := report(t, false)
		for _, want := range []struct {
			kind               string
			approvals, rejects int
			rate               string
		}{
			{authorkind.Polecat, 1, 1, "50.0%"},
			{authorkind.Crew, 0, 1, "100.0%"},
		} {
			row := omStatsTableRow(text, "Author", want.kind)
			if row == nil {
				t.Fatalf("Author table lacks the %s row:\n%s", want.kind, text)
			}
			if len(row) != 4 || row[1] != strconv.Itoa(want.approvals) || row[2] != strconv.Itoa(want.rejects) || row[3] != want.rate {
				t.Errorf("%s row = %v, want %d approvals, %d rejections at %s", want.kind, row, want.approvals, want.rejects, want.rate)
			}
		}
	})

	t.Run("--json carries authors", func(t *testing.T) {
		t.Parallel()
		raw := report(t, true)
		var stats land.OmStats
		if err := json.Unmarshal([]byte(raw), &stats); err != nil {
			t.Fatalf("--json output does not decode: %v\n%s", err, raw)
		}
		want := []land.OmStatsGroup{
			{Name: authorkind.Polecat, Approvals: 1, Rejections: 1, Rate: 0.5},
			{Name: authorkind.Crew, Approvals: 0, Rejections: 1, Rate: 1},
		}
		if len(stats.Authors) != len(want) {
			t.Fatalf("authors = %+v, want %+v", stats.Authors, want)
		}
		for i, w := range want {
			if stats.Authors[i] != w {
				t.Errorf("author %d = %+v, want %+v", i, stats.Authors[i], w)
			}
		}
	})
}

// TestLandOmStatsCrewBirdTable is gt-kg8rha's command acceptance: the text
// form prints the crew records per bird in the Agent table's layout and after
// the Author table, a range with no crew record prints no such table, and
// --json carries the same rows under "crew_birds". The no-crew row is what a
// mutation that prints the table without a crew record turns red.
func TestLandOmStatsCrewBirdTable(t *testing.T) {
	t.Parallel()
	day := func(hour int) time.Time { return time.Date(2026, 10, 16, hour, 0, 0, 0, time.Local) }
	polecatRec := land.ReviewRecord{
		BeadID: "gt-p1", Rig: "gastown", Attempt: 1, Score: 0.90, Verdict: land.VerdictApprove,
		Agent: "opus", Branch: "polecat/dag/gt-p1", AuthorKind: authorkind.Polecat, At: day(13),
	}
	crewRecs := []land.ReviewRecord{
		{BeadID: "gt-c1", Rig: "gastown", Attempt: 1, Score: 0.90, Verdict: land.VerdictApprove,
			Agent: "sonnet", Branch: "crew/nux/gt-c1", AuthorKind: authorkind.Crew, At: day(10)},
		{BeadID: "gt-c2", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: land.VerdictRequestChanges,
			Agent: "sonnet", Branch: "crew/nux/gt-c2", AuthorKind: authorkind.Crew, Findings: 1, At: day(11)},
		{BeadID: "gt-c3", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: land.VerdictRequestChanges,
			Agent: "haiku", Branch: "crew/dag/gt-c3", AuthorKind: authorkind.Crew, Findings: 2, At: day(12)},
		// A polecat record in the same range has no bird and is in no bird row.
		polecatRec,
	}
	report := func(t *testing.T, recs []land.ReviewRecord, asJSON bool) string {
		t.Helper()
		var lines []string
		for _, rec := range recs {
			line, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal %+v: %v", rec, err)
			}
			lines = append(lines, string(line))
		}
		fake := &landOmStatsFakeFS{files: map[string]string{
			"/town/reviews.jsonl": strings.Join(lines, "\n") + "\n",
			"/town/daemon.log":    "",
		}}
		var out bytes.Buffer
		if err := landOmStatsReportFrom(fake, omStatsNoTown(t), &out, "", "/town/reviews.jsonl", "/town/daemon.log", asJSON, false); err != nil {
			t.Fatalf("landOmStatsReportFrom: %v", err)
		}
		return out.String()
	}

	t.Run("the Crew bird table follows the Author table", func(t *testing.T) {
		t.Parallel()
		text := report(t, crewRecs, false)
		author, crewBird := omStatsTableStart(text, "Author"), omStatsTableStart(text, "Crew bird")
		if author < 0 || crewBird < 0 {
			t.Fatalf("Author table at %d, Crew bird table at %d, want both:\n%s", author, crewBird, text)
		}
		if crewBird < author {
			t.Errorf("the Crew bird table at %d precedes the Author table at %d:\n%s", crewBird, author, text)
		}
	})

	t.Run("each bird's counts", func(t *testing.T) {
		t.Parallel()
		text := report(t, crewRecs, false)
		for _, want := range []struct {
			bird               string
			approvals, rejects int
			rate               string
		}{
			{"nux", 1, 1, "50.0%"},
			{"dag", 0, 1, "100.0%"},
		} {
			row := omStatsTableRow(text, "Crew bird", want.bird)
			if row == nil {
				t.Fatalf("Crew bird table lacks the %s row:\n%s", want.bird, text)
			}
			if len(row) != 4 || row[1] != strconv.Itoa(want.approvals) || row[2] != strconv.Itoa(want.rejects) || row[3] != want.rate {
				t.Errorf("%s row = %v, want %d approvals, %d rejections at %s", want.bird, row, want.approvals, want.rejects, want.rate)
			}
		}
		if row := omStatsTableRow(text, "Crew bird", authorkind.Polecat); row != nil {
			t.Errorf("the Crew bird table carries the polecat row %v, want crew records only:\n%s", row, text)
		}
	})

	t.Run("no crew record prints no Crew bird table", func(t *testing.T) {
		t.Parallel()
		text := report(t, []land.ReviewRecord{polecatRec}, false)
		if omStatsTableStart(text, "Author") < 0 {
			t.Fatalf("the fixture lost its Author table:\n%s", text)
		}
		if start := omStatsTableStart(text, "Crew bird"); start >= 0 {
			t.Errorf("a range without a crew record printed a Crew bird table at %d:\n%s", start, text)
		}
	})

	t.Run("--json carries crew_birds", func(t *testing.T) {
		t.Parallel()
		raw := report(t, crewRecs, true)
		if !strings.Contains(raw, `"crew_birds"`) {
			t.Fatalf("--json output does not name crew_birds:\n%s", raw)
		}
		var stats land.OmStats
		if err := json.Unmarshal([]byte(raw), &stats); err != nil {
			t.Fatalf("--json output does not decode: %v\n%s", err, raw)
		}
		want := []land.OmStatsGroup{
			{Name: "nux", Approvals: 1, Rejections: 1, Rate: 0.5},
			{Name: "dag", Approvals: 0, Rejections: 1, Rate: 1},
		}
		if len(stats.CrewBirds) != len(want) {
			t.Fatalf("crew birds = %+v, want %+v", stats.CrewBirds, want)
		}
		for i, w := range want {
			if stats.CrewBirds[i] != w {
				t.Errorf("crew bird %d = %+v, want %+v", i, stats.CrewBirds[i], w)
			}
		}
	})
}
