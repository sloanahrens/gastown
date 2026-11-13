package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMetricsStoryPointsReadsTodaysLandings: the storypoints step reads the
// landings table the extract wrote and prints the shadow report over it. The
// fixture's beads carry no story-point labels, so the report says it has none.
func TestMetricsStoryPointsReadsTodaysLandings(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	town.out.Reset()
	code, err := town.run(t, CommandStoryPoints)
	if err != nil {
		t.Fatalf("storypoints: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	want := "story points: 0 estimated landings with a measured time; the report needs 60\n"
	if got := town.out.String(); got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// TestMetricsStoryPointsNeedsTheExtract: with no extract run for the day there is
// no landings table to read, and the step says which step to run first.
func TestMetricsStoryPointsNeedsTheExtract(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	code, err := town.run(t, CommandStoryPoints)
	if err == nil {
		t.Fatalf("storypoints returned no error with no landings table (code %d)", code)
	}
	if !strings.Contains(err.Error(), CommandExtract) {
		t.Errorf("err = %v, want it to name the extract step", err)
	}
}

// storyPointsTodayReport is the report the fixture's table prints: its beads
// carry no story-point labels.
const storyPointsTodayReport = "story points: 0 estimated landings with a measured time; the report needs 60\n"

// writeExtractAt runs the extract into the metrics root's directory for day,
// leaving the fixture's own out directory where it was, and returns that
// directory. It is how a case builds an older extract today's run falls back to.
func (f *commandTown) writeExtractAt(t *testing.T, day string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(f.outDir()), day)
	was := f.real.Extract.OutDir
	f.real.Extract.OutDir = dir
	if _, err := f.run(t, CommandExtract); err != nil {
		t.Fatalf("extract into %s: %v", day, err)
	}
	f.real.Extract.OutDir = was
	f.out.Reset()
	return dir
}

// TestStorypointsFallsBackToNewestExtract: with no extract for today the step
// reads the newest dated extract that holds a landings table, and its first
// line names that day. A dated directory without the table is not an extract,
// and neither is a directory the date layout does not name.
func TestStorypointsFallsBackToNewestExtract(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	root := filepath.Dir(town.outDir())
	town.writeExtractAt(t, "2026-10-20")
	town.writeExtractAt(t, "2026-10-22")
	// A newer day the extract never wrote, a file named for a day, and a
	// directory whose name is not a day. The file is newer than every extract,
	// so only its shape keeps it out; the last holds a ragged table that would
	// stop the step if it were read.
	if err := os.MkdirAll(filepath.Join(root, "2026-10-21"), 0o755); err != nil {
		t.Fatalf("make an empty day: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "2026-10-24"), nil, 0o644); err != nil {
		t.Fatalf("make a day named by a file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "latest"), 0o755); err != nil {
		t.Fatalf("make latest: %v", err)
	}
	ragged := []byte("bead,rig,sp,sp_actual\r\ngt-1,gastown\r\n")
	if err := os.WriteFile(filepath.Join(root, "latest", "landings.csv"), ragged, 0o644); err != nil {
		t.Fatalf("write latest table: %v", err)
	}
	if _, err := town.run(t, CommandStoryPoints); err != nil {
		t.Fatalf("storypoints: %v", err)
	}
	want := "story points: today's extract is missing; reading the 2026-10-22 extract\n" + storyPointsTodayReport
	if got := town.out.String(); got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// TestStorypointsUsesToday: with today's extract present the step reads it and
// prints no fallback line, even when an older day holds a table it could not
// read.
func TestStorypointsUsesToday(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	older := filepath.Join(filepath.Dir(town.outDir()), "2026-10-20")
	if err := os.MkdirAll(older, 0o755); err != nil {
		t.Fatalf("make an older day: %v", err)
	}
	ragged := []byte("bead,rig,sp,sp_actual\r\ngt-1,gastown\r\n")
	if err := os.WriteFile(filepath.Join(older, "landings.csv"), ragged, 0o644); err != nil {
		t.Fatalf("write older table: %v", err)
	}
	town.out.Reset()
	if _, err := town.run(t, CommandStoryPoints); err != nil {
		t.Fatalf("storypoints: %v", err)
	}
	if got := town.out.String(); got != storyPointsTodayReport {
		t.Errorf("out = %q, want today's report alone %q", got, storyPointsTodayReport)
	}
}

// TestExtractReadsToday: the extract still writes today's directory, even with
// an older day's extract beside it, and leaves that older extract as it was.
func TestExtractReadsToday(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	older := town.writeExtractAt(t, "2026-10-20")
	before := readFile(t, filepath.Join(older, "landings.csv"))
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	requireFile(t, filepath.Join(town.outDir(), "landings.csv"))
	if got := readFile(t, filepath.Join(older, "landings.csv")); got != before {
		t.Error("the extract rewrote an older day's landings table")
	}
}
