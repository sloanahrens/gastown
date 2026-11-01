package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/metrics"
)

// metricsRepeatsFakeFS is the in-memory filesystem the tests hand the command:
// Open and nothing else, so the command has no way to write. opened records
// every path the command reached, errs names the paths whose open fails for a
// reason other than a missing file, and readErrs names the paths that open and
// then fail on their first read.
type metricsRepeatsFakeFS struct {
	files    map[string]string
	errs     map[string]error
	readErrs map[string]error
	opened   []string
}

func (f *metricsRepeatsFakeFS) Open(name string) (io.ReadCloser, error) {
	f.opened = append(f.opened, name)
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	if err, ok := f.readErrs[name]; ok {
		return metricsRepeatsFailingReader{err: err}, nil
	}
	data, ok := f.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

// metricsRepeatsFailingReader is a log that opens and fails on its first read.
type metricsRepeatsFailingReader struct{ err error }

func (f metricsRepeatsFailingReader) Read([]byte) (int, error) { return 0, f.err }
func (metricsRepeatsFailingReader) Close() error               { return nil }

// metricsRepeatsFailWriter is an output that refuses every write.
type metricsRepeatsFailWriter struct{}

func (metricsRepeatsFailWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// metricsRepeatsFixtureLines renders three records: two rounds of one bead,
// whose second restates a finding of the first on the same file, and one
// count-only record written before the log kept finding_text.
func metricsRepeatsFixtureLines(t *testing.T) string {
	t.Helper()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	recs := []land.ReviewRecord{
		{
			BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.4, Verdict: land.VerdictRequestChanges, Findings: 2,
			FindingText: []land.ReviewFindingText{
				{File: "internal/land/gate.go", Title: "the gate ignores a failed read", Claim: "a failed read is reported as no findings"},
				{File: "internal/land/note.go", Title: "the note renames a column", Claim: "the reader shows the old name"},
			},
			At: day(20),
		},
		{
			BeadID: "gt-a", Rig: "gastown", Attempt: 2, Score: 0.5, Verdict: land.VerdictRequestChanges, Findings: 1,
			FindingText: []land.ReviewFindingText{
				{File: "internal/land/gate.go", Title: "the gate ignores a failed read", Claim: "a failed read is still reported as no findings"},
			},
			At: day(21),
		},
		{BeadID: "gt-b", Rig: "longeye", Attempt: 1, Score: 0.6, Verdict: land.VerdictRequestChanges, Findings: 3, At: day(21)},
	}
	var lines []string
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		lines = append(lines, string(line))
	}
	return strings.Join(lines, "\n") + "\n"
}

// runMetricsRepeatsOutput runs the command body against a town whose review log
// holds the fixture, and returns what it printed and the path it read.
func runMetricsRepeatsOutput(t *testing.T, since string, asJSON bool) (string, *metricsRepeatsFakeFS, string) {
	t.Helper()
	townRoot := t.TempDir()
	path := land.TownReviewsFile(townRoot).Path
	fake := &metricsRepeatsFakeFS{files: map[string]string{path: metricsRepeatsFixtureLines(t)}}
	var out bytes.Buffer
	if err := metricsRepeatsReportFrom(fake, func() (string, error) { return townRoot, nil }, &out, since, asJSON); err != nil {
		t.Fatalf("metricsRepeatsReportFrom: %v", err)
	}
	return out.String(), fake, path
}

// TestMetricsRepeatsCommandRegistered holds gt metrics to carrying the report
// as a subcommand with its two flags, refusing a positional argument.
func TestMetricsRepeatsCommandRegistered(t *testing.T) {
	t.Parallel()
	sub, _, err := metricsCmd.Find([]string{"repeats"})
	if err != nil {
		t.Fatalf("gt metrics has no repeats subcommand: %v", err)
	}
	if sub.Name() != "repeats" {
		t.Errorf("found %q, want repeats", sub.Name())
	}
	for _, flag := range []string{"since", "json"} {
		if sub.Flags().Lookup(flag) == nil {
			t.Errorf("repeats has no --%s flag", flag)
		}
	}
	if err := sub.Args(sub, []string{"extra"}); err == nil {
		t.Errorf("repeats accepts a positional argument, want it refused")
	}
}

// TestMetricsRepeatsJSON is gt-657q2's command acceptance: the JSON carries the
// shares the fixture holds, and the command read the town's default review log.
func TestMetricsRepeatsJSON(t *testing.T) {
	t.Parallel()
	raw, fake, path := runMetricsRepeatsOutput(t, "", true)

	var report metrics.RepeatReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("--json output does not decode: %v\n%s", err, raw)
	}
	if report.Findings != 3 || report.Repeated != 1 || report.NoText != 1 || report.Skipped != 0 {
		t.Errorf("report = %+v, want 3 findings, 1 repeated, 1 no_text, 0 skipped", report)
	}
	if want := 1.0 / 3.0; report.RepeatShare != want {
		t.Errorf("repeat share = %v, want %v", report.RepeatShare, want)
	}
	if len(report.Beads) != 1 || report.Beads[0].Bead != "gt-a" || report.Beads[0].Rig != "gastown" ||
		report.Beads[0].Findings != 3 || report.Beads[0].Repeated != 1 {
		t.Errorf("beads = %+v, want gt-a alone with 1 of 3", report.Beads)
	}

	if len(fake.opened) != 1 || fake.opened[0] != path {
		t.Errorf("command opened %v, want the town's review log %s", fake.opened, path)
	}
}

// TestMetricsRepeatsText renders the same fixture as the table: the share, the
// bead that repeats, and the records whose text the share does not cover.
func TestMetricsRepeatsText(t *testing.T) {
	t.Parallel()
	text, _, _ := runMetricsRepeatsOutput(t, "", false)
	for _, want := range []string{
		"Repeated om findings, all rounds",
		"Findings 3, repeated 1 (33.3%)",
		"Beads repeating most",
		"gt-a",
		"1 record(s) named a finding count and no finding text",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("table does not carry %q:\n%s", want, text)
		}
	}
}

// TestMetricsRepeatsSinceDropsEarlierRounds: a round before --since is not a
// baseline, so a restatement whose earlier round is out of range is not a
// repeat.
func TestMetricsRepeatsSinceDropsEarlierRounds(t *testing.T) {
	t.Parallel()
	text, _, _ := runMetricsRepeatsOutput(t, "2026-10-21", false)
	for _, want := range []string{
		"Repeated om findings since 2026-10-21",
		"Findings 1, repeated 0 (0.0%)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("table does not carry %q:\n%s", want, text)
		}
	}
}

// TestMetricsRepeatsMissingLogIsEmptyReport: a town that has landed nothing has
// no review log, which is an empty report and not an error.
func TestMetricsRepeatsMissingLogIsEmptyReport(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	fake := &metricsRepeatsFakeFS{}
	var out bytes.Buffer
	if err := metricsRepeatsReportFrom(fake, func() (string, error) { return townRoot, nil }, &out, "", true); err != nil {
		t.Fatalf("metricsRepeatsReportFrom: %v", err)
	}
	var report metrics.RepeatReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("--json output does not decode: %v", err)
	}
	if report.Findings != 0 || report.Repeated != 0 || len(report.Beads) != 0 {
		t.Errorf("report = %+v, want an empty one", report)
	}
	if raw := out.String(); !strings.Contains(raw, `"beads": []`) {
		t.Errorf("empty report does not carry an empty beads list:\n%s", raw)
	}
	if report.NoLogAt == "" {
		t.Error("the JSON report does not say the log was missing: a wrong town reads as a quiet one")
	}
}

// The table says the log is missing, with its path, so an empty report from the
// wrong town is not mistaken for a clean one (gt-algrd).
func TestMetricsRepeatsMissingLogIsSaidInTheTable(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var out bytes.Buffer
	if err := metricsRepeatsReportFrom(&metricsRepeatsFakeFS{}, func() (string, error) { return townRoot, nil }, &out, "", false); err != nil {
		t.Fatalf("metricsRepeatsReportFrom: %v", err)
	}
	if !strings.Contains(out.String(), "no review log at "+townRoot) {
		t.Errorf("table = %q, want it to name the missing log under %s", out.String(), townRoot)
	}
}

// TestMetricsRepeatsUnreadableLogFails: a log that exists and cannot be read
// fails the report rather than passing as an empty one.
func TestMetricsRepeatsUnreadableLogFails(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := land.TownReviewsFile(townRoot).Path
	fake := &metricsRepeatsFakeFS{errs: map[string]error{path: errors.New("permission denied")}}
	var out bytes.Buffer
	err := metricsRepeatsReportFrom(fake, func() (string, error) { return townRoot, nil }, &out, "", false)
	if err == nil {
		t.Fatalf("an unreadable review log did not fail the report:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %v does not name %s", err, path)
	}
}

// TestMetricsRepeatsRejectsUnreadableSince: a --since that is not a day fails
// the report rather than reporting every round.
func TestMetricsRepeatsRejectsUnreadableSince(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var out bytes.Buffer
	err := metricsRepeatsReportFrom(&metricsRepeatsFakeFS{}, func() (string, error) { return townRoot, nil }, &out, "yesterday", false)
	if err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("error = %v, want a --since refusal", err)
	}
	if out.Len() != 0 {
		t.Errorf("report wrote %q for an unreadable --since", out.String())
	}
}

// TestMetricsRepeatsFailsWhenTownUnresolved: a town the report cannot resolve
// fails it, since without one there is no log to read.
func TestMetricsRepeatsFailsWhenTownUnresolved(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := metricsRepeatsReportFrom(&metricsRepeatsFakeFS{},
		func() (string, error) { return "", errors.New("no town") }, &out, "", false)
	if err == nil || !strings.Contains(err.Error(), "resolving the town") {
		t.Fatalf("error = %v, want the town resolution failure", err)
	}
}

// TestMetricsRepeatsFailsWhenLogReadFails: a log that opens and then fails to
// read fails the report rather than passing as an empty one.
func TestMetricsRepeatsFailsWhenLogReadFails(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := land.TownReviewsFile(townRoot).Path
	fake := &metricsRepeatsFakeFS{readErrs: map[string]error{path: errors.New("disk error")}}
	var out bytes.Buffer
	err := metricsRepeatsReportFrom(fake, func() (string, error) { return townRoot, nil }, &out, "", false)
	if err == nil || !strings.Contains(err.Error(), "reading "+path) {
		t.Fatalf("error = %v, want a read failure naming %s", err, path)
	}
}

// TestMetricsRepeatsFailsWhenOutputFails: an output that refuses the report
// fails the command rather than reporting success.
func TestMetricsRepeatsFailsWhenOutputFails(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := land.TownReviewsFile(townRoot).Path
	fake := &metricsRepeatsFakeFS{files: map[string]string{path: metricsRepeatsFixtureLines(t)}}
	err := metricsRepeatsReportFrom(fake, func() (string, error) { return townRoot, nil }, metricsRepeatsFailWriter{}, "", true)
	if err == nil || !strings.Contains(err.Error(), "writing the report") {
		t.Fatalf("error = %v, want the write failure", err)
	}
}
