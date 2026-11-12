package metrics

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/dispatch"
)

// jevLogLine is one dispatch.jsonl line for bead, logged at ts, with the
// fits-twenty-minutes probability and the size score Jev answered.
func jevLogLine(t *testing.T, bead, ts string, fits, size float64) string {
	t.Helper()
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse %q: %v", ts, err)
	}
	data, err := json.Marshal(dispatch.Line{
		TS:      at,
		Bead:    bead,
		Answers: dispatch.Answers{FitsTwentyMinutes: fits, Size: dispatch.SizeAnswer{Score: size}},
	})
	if err != nil {
		t.Fatalf("marshal dispatch line: %v", err)
	}
	return string(data)
}

// jevLogFile writes lines as a dispatch log under the test's temp directory and
// returns its path. No lines makes an empty log, which reads as one with no
// answers.
func jevLogFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	data := strings.Join(lines, "\n")
	if len(lines) > 0 {
		data += "\n"
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write the dispatch log: %v", err)
	}
	return path
}

// TestBuildLandingsJevCells checks a landing whose bead has a Jev line carries
// its fits-twenty probability and size score, and one with no line keeps both
// cells empty rather than reading as a zero answer.
func TestBuildLandingsJevCells(t *testing.T) {
	t.Parallel()
	jev := map[string]dispatch.Line{
		"gt-1": {Answers: dispatch.Answers{FitsTwentyMinutes: 0.75, Size: dispatch.SizeAnswer{Score: 3}}},
	}
	rows, err := BuildLandings(context.Background(), tablesLedger, tablesRework, fakeDiffSource{},
		tablesBeads(), tablesUsage, tablesOverrun, tablesEras, tablesShaped, StageStamps{}, jev, &Dropped{})
	if err != nil {
		t.Fatalf("BuildLandings: %v", err)
	}
	byBead := make(map[string]Row, len(rows))
	for _, row := range rows {
		byBead[row["bead"]] = row
	}
	with := byBead["gt-1"]
	if got := with["jev_fits20"]; got != "0.75" {
		t.Errorf("gt-1 jev_fits20 = %q, want 0.75", got)
	}
	if got := with["jev_size"]; got != "3.0" {
		t.Errorf("gt-1 jev_size = %q, want 3.0", got)
	}
	without := byBead["gt-2"]
	if got := without["jev_fits20"]; got != "" {
		t.Errorf("gt-2 jev_fits20 = %q, want an empty cell for a bead with no line", got)
	}
	if got := without["jev_size"]; got != "" {
		t.Errorf("gt-2 jev_size = %q, want an empty cell for a bead with no line", got)
	}
}

// TestExtractJevColumnsCarryTheBeadsLine checks the extract reads the dispatch
// log through its path into the landings table: the bead with a line gets its
// answers, the bead with none gets empty cells.
func TestExtractJevColumnsCarryTheBeadsLine(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.JevLog = jevLogFile(t, jevLogLine(t, "gt-a", "2026-10-15T00:00:00Z", 0.4, 2))
	res := extractRun(t, env)
	_, by := extractLandings(t, files)
	if got := by["gt-a"]["jev_fits20"]; got != "0.4" {
		t.Errorf("gt-a jev_fits20 = %q, want 0.4", got)
	}
	if got := by["gt-a"]["jev_size"]; got != "2.0" {
		t.Errorf("gt-a jev_size = %q, want 2.0", got)
	}
	if got := by["gt-b"]["jev_fits20"]; got != "" {
		t.Errorf("gt-b jev_fits20 = %q, want an empty cell for a bead with no line", got)
	}
	if got := by["gt-b"]["jev_size"]; got != "" {
		t.Errorf("gt-b jev_size = %q, want an empty cell for a bead with no line", got)
	}
	if len(res.Footer.Gaps) != 0 {
		t.Errorf("gaps = %v, want none for a log that was read", res.Footer.Gaps)
	}
}

// TestExtractJevColumnsUseTheEarliestLinePerBead checks a bead logged twice
// takes the answer of its earliest line, even when a later line comes first in
// the file.
func TestExtractJevColumnsUseTheEarliestLinePerBead(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	// The earliest line sits in the middle of the file, so first-in-file and
	// last-in-file both read a different answer.
	env.JevLog = jevLogFile(t,
		jevLogLine(t, "gt-a", "2026-10-20T00:00:00Z", 0.2, 5),
		jevLogLine(t, "gt-a", "2026-10-15T00:00:00Z", 0.9, 2),
		jevLogLine(t, "gt-a", "2026-10-18T00:00:00Z", 0.5, 3),
	)
	extractRun(t, env)
	_, by := extractLandings(t, files)
	if got := by["gt-a"]["jev_fits20"]; got != "0.9" {
		t.Errorf("gt-a jev_fits20 = %q, want 0.9 from the earliest line", got)
	}
	if got := by["gt-a"]["jev_size"]; got != "2.0" {
		t.Errorf("gt-a jev_size = %q, want 2.0 from the earliest line", got)
	}
}

// TestExtractMissingJevLogIsAFooterGapNotAnError checks a log that does not
// exist leaves the Jev columns empty and is named once in the footer, and the
// extract still writes its files.
func TestExtractMissingJevLogIsAFooterGapNotAnError(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.JevLog = filepath.Join(t.TempDir(), "no-such-dispatch.jsonl")
	res := extractRun(t, env)
	if !files.has(landingsName) {
		t.Fatalf("%s not written for a missing Jev log", landingsName)
	}
	_, by := extractLandings(t, files)
	if got := by["gt-a"]["jev_fits20"]; got != "" {
		t.Errorf("gt-a jev_fits20 = %q, want an empty cell for a missing log", got)
	}
	var named int
	for _, gap := range res.Footer.Gaps {
		if gap[0] == jevLogSource {
			named++
		}
	}
	if named != 1 {
		t.Errorf("footer gaps %v name the Jev log %d times, want once", res.Footer.Gaps, named)
	}
}

// TestLoadJevFirstNamesMalformedLines checks a line that does not parse is
// skipped and named as a gap, while the lines that parse still answer.
func TestLoadJevFirstNamesMalformedLines(t *testing.T) {
	t.Parallel()
	path := jevLogFile(t,
		"not json",
		jevLogLine(t, "gt-a", "2026-10-15T00:00:00Z", 0.4, 2),
	)
	dropped := &Dropped{}
	first, err := LoadJevFirst(path, dropped)
	if err != nil {
		t.Fatalf("LoadJevFirst: %v", err)
	}
	if got := first["gt-a"].Answers.FitsTwentyMinutes; got != 0.4 {
		t.Errorf("gt-a fits = %v, want the parsed line's 0.4", got)
	}
	if len(dropped.Items) != 1 || dropped.Items[0].Source != jevLogSource ||
		!strings.Contains(dropped.Items[0].Reason, "1 lines did not parse") {
		t.Errorf("dropped = %+v, want one gap naming the Jev log and its 1 malformed line", dropped.Items)
	}
}

// TestExtractRefusesAnUnreadableJevLog checks a log that exists but cannot be
// read stops the extract before it writes a file, rather than reading as a log
// with no answers. A directory stands in for the unreadable file.
func TestExtractRefusesAnUnreadableJevLog(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.JevLog = t.TempDir()
	if _, err := Extract(context.Background(), env); err == nil {
		t.Fatal("Extract succeeded over an unreadable Jev log, want a refusal")
	}
	assertNoFilesWritten(t, files)
}
