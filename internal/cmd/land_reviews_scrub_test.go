package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// reviewsScrubToken is built from parts: no token-shaped literal in this file.
var reviewsScrubToken = "AK" + "IA" + strings.Repeat("Q", 16)

// reviewsScrubLogBytes is a three-row log; the middle row's finding holds the token.
func reviewsScrubLogBytes(t *testing.T) []byte {
	t.Helper()
	recs := []land.ReviewRecord{
		{BeadID: "gt-keep1", Verdict: land.VerdictApprove},
		{BeadID: "gt-slit1", Verdict: land.VerdictRequestChanges, Findings: 1,
			FindingText: []land.ReviewFindingText{{Claim: "it logs " + reviewsScrubToken + " verbatim"}}},
		{BeadID: "gt-keep2", Verdict: land.VerdictApprove},
	}
	var b bytes.Buffer
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", rec, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func writeReviewsScrubFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, land.ReviewsFileName)
	if err := os.WriteFile(path, reviewsScrubLogBytes(t), 0o600); err != nil {
		t.Fatalf("writing the fixture log: %v", err)
	}
	return path
}

// reviewsScrubNoTown fails the test if the command resolves the town.
func reviewsScrubNoTown(t *testing.T) func() (string, error) {
	return func() (string, error) {
		t.Errorf("the command resolved the town with --records named")
		return "", nil
	}
}

// assertReviewsScrubNoToken fails if text holds the token or half of it.
func assertReviewsScrubNoToken(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, reviewsScrubToken) {
		t.Fatal("the output holds the planted token")
	}
	if n := len(reviewsScrubToken) / 2; strings.Contains(text, reviewsScrubToken[:n]) {
		t.Fatalf("the output holds the first %d bytes of the planted token", n)
	}
}

// TestLandReviewsScrubDryRunPrintsCountsAndIDs: counts and the changed bead id, no row text, the log unchanged.
func TestLandReviewsScrubDryRunPrintsCountsAndIDs(t *testing.T) {
	t.Parallel()
	path := writeReviewsScrubFixture(t, t.TempDir())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture log: %v", err)
	}
	var out bytes.Buffer
	if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, false, false); err != nil {
		t.Fatalf("landReviewsScrubReportFrom: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the dry run printed %d line(s), want 2:\n%s", len(lines), out.String())
	}
	if want := "reviews-scrub (dry run): 3 rows, 1 would change, 1 words, oversize 0, skipped 0"; lines[0] != want {
		t.Errorf("counts line = %q, want %q", lines[0], want)
	}
	if want := "beads: gt-slit1"; lines[1] != want {
		t.Errorf("beads line = %q, want %q", lines[1], want)
	}
	if strings.Contains(out.String(), "verbatim") {
		t.Errorf("the dry run printed row text:\n%s", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log after the dry run: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the dry run changed the log")
	}
}

// TestLandReviewsScrubApplyRewritesTheLog: --apply rewrites the flagged word out
// of the log, and a second run finds nothing to change.
func TestLandReviewsScrubApplyRewritesTheLog(t *testing.T) {
	t.Parallel()
	path := writeReviewsScrubFixture(t, t.TempDir())
	var first bytes.Buffer
	if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &first, path, true, false); err != nil {
		t.Fatalf("the first --apply run: %v", err)
	}
	if want := "reviews-scrub (applied): 3 rows, 1 changed, 1 words, oversize 0, skipped 0\nbeads: gt-slit1\n"; first.String() != want {
		t.Errorf("the first --apply run printed %q, want %q", first.String(), want)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the rewritten log: %v", err)
	}
	assertReviewsScrubNoToken(t, string(rewritten))
	if n := bytes.Count(rewritten, []byte("\n")); n != 3 {
		t.Errorf("the rewritten log has %d line(s), want 3", n)
	}

	var second bytes.Buffer
	if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &second, path, true, false); err != nil {
		t.Fatalf("the second --apply run: %v", err)
	}
	if !strings.Contains(second.String(), "3 rows, 0 changed, 0 words") {
		t.Errorf("the second --apply run printed %q, want zero changes", second.String())
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log after the second run: %v", err)
	}
	if !bytes.Equal(again, rewritten) {
		t.Error("the second --apply run changed the log")
	}
}

// TestLandReviewsScrubMissingLogIsAnError: a missing log is an error naming its
// path in both modes, prints nothing, and the run creates no file.
func TestLandReviewsScrubMissingLogIsAnError(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name  string
		apply bool
	}{{"dry run", false}, {"apply", true}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), land.ReviewsFileName)
			var out bytes.Buffer
			err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, mode.apply, false)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("a missing log returned %v, want an error naming %s", err, path)
			}
			if out.Len() != 0 {
				t.Errorf("a failed run printed %q", out.String())
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the run created the missing log: stat error %v", err)
			}
		})
	}
}

// TestLandReviewsScrubOutputHoldsNoToken: no part of the token, in a finding or as a bead id (scrubbed first), in either mode.
func TestLandReviewsScrubOutputHoldsNoToken(t *testing.T) {
	t.Parallel()
	idRow, err := json.Marshal(land.ReviewRecord{BeadID: reviewsScrubToken, Verdict: land.VerdictRequestChanges, Findings: 1,
		FindingText: []land.ReviewFindingText{{Claim: "it logs " + reviewsScrubToken}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, tc := range []struct {
		name string
		log  []byte
	}{
		{"in the finding text", reviewsScrubLogBytes(t)},
		{"as the bead id", append(idRow, '\n')},
	} {
		for _, mode := range []struct {
			name  string
			apply bool
		}{{"dry run", false}, {"apply", true}} {
			t.Run(tc.name+", "+mode.name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), land.ReviewsFileName)
				if err := os.WriteFile(path, tc.log, 0o600); err != nil {
					t.Fatalf("writing the fixture log: %v", err)
				}
				var out bytes.Buffer
				if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, mode.apply, false); err != nil {
					t.Fatalf("landReviewsScrubReportFrom: %v", err)
				}
				assertReviewsScrubNoToken(t, out.String())
			})
		}
	}
}

// TestLandReviewsScrubReadsTheTownDefault: with no --records the command reads
// the town's own review log.
func TestLandReviewsScrubReadsTheTownDefault(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := land.TownReviewsFile(town).Path
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("making the landings directory: %v", err)
	}
	if err := os.WriteFile(logPath, reviewsScrubLogBytes(t), 0o600); err != nil {
		t.Fatalf("writing the town's log: %v", err)
	}
	var out bytes.Buffer
	if err := landReviewsScrubReportFrom(func() (string, error) { return town, nil }, &out, "", false, false); err != nil {
		t.Fatalf("landReviewsScrubReportFrom: %v", err)
	}
	if !strings.Contains(out.String(), "3 rows, 1 would change") {
		t.Errorf("the town's log was not the one read:\n%s", out.String())
	}
}

// TestLandReviewsScrubSurfacesAnOpenError: a directory as the records path is an error.
func TestLandReviewsScrubSurfacesAnOpenError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, t.TempDir(), false, false)
	if err == nil {
		t.Fatal("a directory as the records path returned no error")
	}
	if out.Len() != 0 {
		t.Errorf("a failed run printed %q", out.String())
	}
}

// TestLandReviewsScrubFailsWhenTheTownCannotBeResolved: the default log's town.
func TestLandReviewsScrubFailsWhenTheTownCannotBeResolved(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := landReviewsScrubReportFrom(func() (string, error) { return "", errors.New("no town") }, &out, "", false, false)
	if err == nil {
		t.Fatal("an unresolvable town returned no error")
	}
	if !strings.Contains(err.Error(), "resolving the town") {
		t.Errorf("error = %v, want it to name the town resolution", err)
	}
	if out.Len() != 0 {
		t.Errorf("a failed run printed %q", out.String())
	}
}

// reviewsScrubFailingWriter fails every write.
type reviewsScrubFailingWriter struct{}

func (reviewsScrubFailingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// TestLandReviewsScrubSurfacesAWriteError: a report the writer refuses is the
// run's error, in the dry run, in --json, and after an --apply.
func TestLandReviewsScrubSurfacesAWriteError(t *testing.T) {
	t.Parallel()
	path := writeReviewsScrubFixture(t, t.TempDir())
	err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), reviewsScrubFailingWriter{}, path, false, false)
	if err == nil || !strings.Contains(err.Error(), "writing the plan") {
		t.Fatalf("a failing writer returned %v, want an error naming the write", err)
	}
	err = landReviewsScrubReportFrom(reviewsScrubNoTown(t), reviewsScrubFailingWriter{}, path, false, true)
	if err == nil || !strings.Contains(err.Error(), "writing the plan") {
		t.Fatalf("a failing writer in JSON returned %v, want an error naming the write", err)
	}
	err = landReviewsScrubReportFrom(reviewsScrubNoTown(t), reviewsScrubFailingWriter{}, path, true, false)
	if err == nil || !strings.Contains(err.Error(), "writing the plan") {
		t.Fatalf("a failing writer after --apply returned %v, want an error naming the write", err)
	}
}

// TestLandReviewsScrubJSON: --json prints the figures as one object with the bead ids,
// an empty list as [], and no part of the planted token (gt-2eh81l).
func TestLandReviewsScrubJSON(t *testing.T) {
	t.Parallel()
	clean := filepath.Join(t.TempDir(), land.ReviewsFileName)
	row, err := json.Marshal(land.ReviewRecord{BeadID: "gt-keep1", Verdict: land.VerdictApprove})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(clean, append(row, '\n'), 0o600); err != nil {
		t.Fatalf("writing the clean log: %v", err)
	}
	t.Run("a log with a row to change", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, writeReviewsScrubFixture(t, t.TempDir()), false, true); err != nil {
			t.Fatalf("landReviewsScrubReportFrom: %v", err)
		}
		assertReviewsScrubNoToken(t, out.String())
		var plan landReviewsScrubPlan
		if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
			t.Fatalf("the JSON output does not parse: %v", err)
		}
		if !plan.DryRun || plan.Rows != 3 || plan.WouldChange != 1 || plan.Words != 1 || !slices.Equal(plan.Beads, []string{"gt-slit1"}) {
			t.Errorf("JSON plan = %+v, want dry_run, 3 rows, 1 changed, 1 word, beads [gt-slit1]", plan)
		}
	})
	t.Run("an apply reports dry_run false", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, writeReviewsScrubFixture(t, t.TempDir()), true, true); err != nil {
			t.Fatalf("landReviewsScrubReportFrom: %v", err)
		}
		assertReviewsScrubNoToken(t, out.String())
		var plan landReviewsScrubPlan
		if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
			t.Fatalf("the JSON output does not parse: %v", err)
		}
		if plan.DryRun || plan.Rows != 3 || plan.WouldChange != 1 || plan.Words != 1 || !slices.Equal(plan.Beads, []string{"gt-slit1"}) {
			t.Errorf("JSON plan = %+v, want dry_run false, 3 rows, 1 changed, 1 word, beads [gt-slit1]", plan)
		}
	})
	t.Run("a log with nothing to change", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, clean, false, true); err != nil {
			t.Fatalf("landReviewsScrubReportFrom: %v", err)
		}
		if !strings.Contains(out.String(), `"beads": []`) {
			t.Errorf("JSON plan = %s, want an empty beads list printed as []", out.String())
		}
	})
}

// reviewsScrubSkippedRow is a row whose flagged word the raw line does not
// hold: the token's middle byte is JSON-escaped, so the scrub skips the row.
func reviewsScrubSkippedRow(t *testing.T, bead string) []byte {
	t.Helper()
	line, err := json.Marshal(land.ReviewRecord{BeadID: bead, Verdict: land.VerdictRequestChanges, Findings: 1,
		FindingText: []land.ReviewFindingText{{Claim: "it logs " + reviewsScrubToken}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mid := len(reviewsScrubToken) / 2
	esc := fmt.Sprintf(`\u%04x`, reviewsScrubToken[mid])
	return bytes.Replace(line, []byte(reviewsScrubToken), []byte(reviewsScrubToken[:mid]+esc+reviewsScrubToken[mid+1:]), 1)
}

// reviewsScrubExitCode is the status the command's error maps to, 0 for nil.
func reviewsScrubExitCode(err error) int {
	if err == nil {
		return 0
	}
	return exitCodeForError(err)
}

// TestLandReviewsScrubSkippedRowNamedAndFailsApply: a skipped row is named by
// its bead id in both modes; --apply exits 1 and a dry run exits 0. The token
// never reaches the output.
func TestLandReviewsScrubSkippedRowNamedAndFailsApply(t *testing.T) {
	t.Parallel()
	log := append(reviewsScrubLogBytes(t), append(reviewsScrubSkippedRow(t, "gt-esc1"), '\n')...)
	for _, tc := range []struct {
		name  string
		apply bool
		want  string
		exit  int
	}{
		{"dry run", false, "reviews-scrub (dry run): 4 rows, 1 would change, 1 words, oversize 0, skipped 1\nbeads: gt-slit1\nskipped beads: gt-esc1\n", 0},
		{"apply", true, "reviews-scrub (applied): 4 rows, 1 changed, 1 words, oversize 0, skipped 1\nbeads: gt-slit1\nskipped beads: gt-esc1\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), land.ReviewsFileName)
			if err := os.WriteFile(path, log, 0o600); err != nil {
				t.Fatalf("writing the fixture log: %v", err)
			}
			var out bytes.Buffer
			err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, tc.apply, false)
			if got := reviewsScrubExitCode(err); got != tc.exit {
				t.Errorf("exit status = %d (error %v), want %d", got, err, tc.exit)
			}
			if out.String() != tc.want {
				t.Errorf("output = %q, want %q", out.String(), tc.want)
			}
			assertReviewsScrubNoToken(t, out.String())
		})
	}
}

// TestLandReviewsScrubCleanLogPrintsNoSkippedLine: a log with no skipped row
// prints no skipped beads line, and --apply exits 0.
func TestLandReviewsScrubCleanLogPrintsNoSkippedLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		apply bool
	}{{"dry run", false}, {"apply", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeReviewsScrubFixture(t, t.TempDir())
			var out bytes.Buffer
			if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, tc.apply, false); err != nil {
				t.Fatalf("landReviewsScrubReportFrom: %v", err)
			}
			if strings.Contains(out.String(), "skipped beads:") {
				t.Errorf("a log with no skipped row printed the skipped line:\n%s", out.String())
			}
			assertReviewsScrubNoToken(t, out.String())
		})
	}
}

// TestLandReviewsScrubSkippedRowJSONFailsApply: --json still carries the figures,
// and --apply over a skipped row exits 1 in that mode too.
func TestLandReviewsScrubSkippedRowJSONFailsApply(t *testing.T) {
	t.Parallel()
	log := append(reviewsScrubLogBytes(t), append(reviewsScrubSkippedRow(t, "gt-esc1"), '\n')...)
	path := filepath.Join(t.TempDir(), land.ReviewsFileName)
	if err := os.WriteFile(path, log, 0o600); err != nil {
		t.Fatalf("writing the fixture log: %v", err)
	}
	var out bytes.Buffer
	err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, true, true)
	if got := reviewsScrubExitCode(err); got != 1 {
		t.Errorf("--json --apply exit status = %d (error %v), want 1", got, err)
	}
	var plan landReviewsScrubPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("the JSON output does not parse: %v", err)
	}
	if plan.DryRun || plan.Skipped != 1 {
		t.Errorf("JSON plan = %+v, want dry_run false and skipped 1", plan)
	}
	assertReviewsScrubNoToken(t, out.String())
}

// reviewsScrubSecondWriteFails accepts the first write and fails the next one.
type reviewsScrubSecondWriteFails struct{ writes int }

func (w *reviewsScrubSecondWriteFails) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("write failed")
	}
	return len(p), nil
}

// TestLandReviewsScrubSurfacesAFailedSkippedLine: a writer that takes the counts
// line and refuses the `skipped beads:` line is the run's error, not a clean exit.
func TestLandReviewsScrubSurfacesAFailedSkippedLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), land.ReviewsFileName)
	log := append(reviewsScrubLogBytes(t), append(reviewsScrubSkippedRow(t, "gt-esc1"), '\n')...)
	if err := os.WriteFile(path, log, 0o600); err != nil {
		t.Fatalf("writing the fixture log: %v", err)
	}

	err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &reviewsScrubSecondWriteFails{}, path, false, false)

	if err == nil || !strings.Contains(err.Error(), "writing the plan") {
		t.Fatalf("a writer failing at the skipped line returned %v, want an error naming the write", err)
	}
}

// TestLandReviewsScrubOversizeLineJoinsIdsAndCount: the line is built from the whole
// ScrubResult (so the caller cannot drop the oversize count), and lists the ids, then the
// oversize count when there is one, and reads as before for ids alone.
func TestLandReviewsScrubOversizeLineJoinsIdsAndCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		ids      []string
		oversize int
		want     string
	}{
		{"ids only", []string{"gt-esc1", "gt-esc2"}, 0, "gt-esc1, gt-esc2"},
		{"ids and one oversize row", []string{"gt-esc1"}, 1, "gt-esc1; 1 oversize row"},
		{"ids and several oversize rows", []string{"gt-esc1"}, 3, "gt-esc1; 3 oversize rows"},
		{"oversize only", nil, 2, "none; 2 oversize rows"},
	} {
		if got := landReviewsScrubSkippedLine(land.ScrubResult{SkippedBeads: tc.ids, Oversize: tc.oversize}); got != tc.want {
			t.Errorf("%s: line = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestLandReviewsScrubOversizeOnlyReportsAndFailsApply: a scrub that skipped no bead
// and met one oversize row names the oversize row on the skipped beads line in both
// modes and the JSON path, and only --apply exits 1: the exit counts oversize alone
// (gt-axehgm). The result is built directly: a real oversize row is a 16 MiB line.
func TestLandReviewsScrubOversizeOnlyReportsAndFailsApply(t *testing.T) {
	t.Parallel()
	res := land.ScrubResult{Rows: 3, Oversize: 1}
	for _, tc := range []struct {
		name         string
		apply, json  bool
		wantLine     string
		wantExitCode int
	}{
		{"dry run", false, false, "skipped beads: none; 1 oversize row\n", 0},
		{"apply", true, false, "skipped beads: none; 1 oversize row\n", 1},
		{"apply, json", true, true, `"oversize": 1`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := writeLandReviewsScrubReport(&out, res, tc.apply, tc.json)
			if got := reviewsScrubExitCode(err); got != tc.wantExitCode {
				t.Errorf("exit status = %d (error %v), want %d", got, err, tc.wantExitCode)
			}
			if !strings.Contains(out.String(), tc.wantLine) {
				t.Errorf("output = %q, want it to hold %q", out.String(), tc.wantLine)
			}
		})
	}
}
