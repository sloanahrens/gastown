package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
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
	if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, false); err != nil {
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

// TestLandReviewsScrubOutputHoldsNoToken: no part of the token, in a finding or as a bead id (scrubbed first).
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
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), land.ReviewsFileName)
			if err := os.WriteFile(path, tc.log, 0o600); err != nil {
				t.Fatalf("writing the fixture log: %v", err)
			}
			var out bytes.Buffer
			if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, path, false); err != nil {
				t.Fatalf("landReviewsScrubReportFrom: %v", err)
			}
			assertReviewsScrubNoToken(t, out.String())
		})
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
	if err := landReviewsScrubReportFrom(func() (string, error) { return town, nil }, &out, "", false); err != nil {
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
	err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, t.TempDir(), false)
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
	err := landReviewsScrubReportFrom(func() (string, error) { return "", errors.New("no town") }, &out, "", false)
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
// run's error.
func TestLandReviewsScrubSurfacesAWriteError(t *testing.T) {
	t.Parallel()
	path := writeReviewsScrubFixture(t, t.TempDir())
	err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), reviewsScrubFailingWriter{}, path, false)
	if err == nil || !strings.Contains(err.Error(), "writing the plan") {
		t.Fatalf("a failing writer returned %v, want an error naming the write", err)
	}
	err = landReviewsScrubReportFrom(reviewsScrubNoTown(t), reviewsScrubFailingWriter{}, path, true)
	if err == nil || !strings.Contains(err.Error(), "writing the plan") {
		t.Fatalf("a failing writer in JSON returned %v, want an error naming the write", err)
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
		if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, writeReviewsScrubFixture(t, t.TempDir()), true); err != nil {
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
	t.Run("a log with nothing to change", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := landReviewsScrubReportFrom(reviewsScrubNoTown(t), &out, clean, true); err != nil {
			t.Fatalf("landReviewsScrubReportFrom: %v", err)
		}
		if !strings.Contains(out.String(), `"beads": []`) {
			t.Errorf("JSON plan = %s, want an empty beads list printed as []", out.String())
		}
	})
}
