package metrics

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// repeatRound is one record of a bead's rework history: the findings om named
// in that round, on one day of October 2026.
func repeatRound(bead string, attempt, day int, findings ...land.ReviewFindingText) land.ReviewRecord {
	return land.ReviewRecord{
		BeadID:      bead,
		Rig:         "gastown",
		Attempt:     attempt,
		Findings:    len(findings),
		FindingText: findings,
		At:          time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC),
	}
}

// TestRepeatedFindingsMatchesAcrossRounds is gt-657q2's first acceptance: a
// finding restated on the same file in a later round counts as a repeat, while
// the same words on another file, in the same round, or on another bead do not.
func TestRepeatedFindingsMatchesAcrossRounds(t *testing.T) {
	t.Parallel()
	gate := land.ReviewFindingText{
		File:  "internal/land/gate.go",
		Title: "the gate ignores a failed read",
		Claim: "a failed read is reported as no findings and the gate passes",
	}
	records := []land.ReviewRecord{
		repeatRound("gt-a", 1, 20,
			gate,
			land.ReviewFindingText{File: "internal/land/gate.go", Title: "the error is dropped", Claim: "the caller never sees the failure"},
		),
		repeatRound("gt-a", 2, 21,
			land.ReviewFindingText{File: "internal/land/gate.go",
				Title: "the gate ignores a failed read",
				Claim: "a failed read still reports no findings and passes the gate"},
			land.ReviewFindingText{File: "internal/metrics/panel.go", Title: gate.Title, Claim: gate.Claim},
			land.ReviewFindingText{File: "internal/land/note.go", Title: "the note renames a column", Claim: "the reader shows the old name"},
			land.ReviewFindingText{File: "internal/land/note.go", Title: "the note renames a column", Claim: "the reader shows the old name"},
		),
		repeatRound("gt-b", 2, 21, land.ReviewFindingText{File: "internal/land/gate.go", Title: gate.Title, Claim: gate.Claim}),
	}

	got := RepeatedFindings(records)
	if got.Findings != 7 || got.Repeated != 1 {
		t.Fatalf("findings/repeated = %d/%d, want 7/1", got.Findings, got.Repeated)
	}
	if want := 1.0 / 7.0; got.RepeatShare != want {
		t.Errorf("repeat share = %v, want %v", got.RepeatShare, want)
	}
	if len(got.Beads) != 1 || got.Beads[0].Bead != "gt-a" || got.Beads[0].Findings != 6 || got.Beads[0].Repeated != 1 {
		t.Errorf("beads = %+v, want gt-a alone with 1 of 6", got.Beads)
	}
	if got.NoText != 0 || got.Skipped != 0 {
		t.Errorf("no_text/skipped = %d/%d, want 0/0", got.NoText, got.Skipped)
	}
}

// TestRepeatedFindingsSkipsRecordsWithoutText is gt-657q2's second acceptance:
// a record that names more findings than it records text for counts in NoText,
// and the findings it does not record are in neither count; an approval, which
// names no findings at all, is not such a record.
func TestRepeatedFindingsSkipsRecordsWithoutText(t *testing.T) {
	t.Parallel()
	records := []land.ReviewRecord{
		{BeadID: "gt-c", Rig: "gastown", Attempt: 1, Findings: 3, At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)},
		{
			BeadID: "gt-c", Rig: "gastown", Attempt: 2, Findings: 2,
			FindingText: []land.ReviewFindingText{{File: "a.go", Title: "one"}},
			At:          time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
		},
		{
			BeadID: "gt-c", Rig: "gastown", Attempt: 3, Findings: 0, Verdict: land.VerdictApprove,
			At: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
		},
	}

	got := RepeatedFindings(records)
	if got.NoText != 2 {
		t.Errorf("no_text = %d, want 2 (the count-only round and the partial one)", got.NoText)
	}
	if got.Findings != 1 || got.Repeated != 0 {
		t.Errorf("findings/repeated = %d/%d, want 1/0", got.Findings, got.Repeated)
	}
	if len(got.Beads) != 0 {
		t.Errorf("beads = %+v, want none", got.Beads)
	}
}

// TestRepeatedFindingsThresholdBoundary pins the threshold's inclusive edge: a
// finding that shares exactly the threshold's share of its words with an
// earlier one repeats, and one under it does not.
func TestRepeatedFindingsThresholdBoundary(t *testing.T) {
	t.Parallel()
	records := []land.ReviewRecord{
		repeatRound("gt-d", 1, 20, land.ReviewFindingText{File: "x.go", Title: "alpha beta gamma delta"}),
		repeatRound("gt-d", 2, 21,
			land.ReviewFindingText{File: "x.go", Title: "alpha beta gamma epsilon"},
			land.ReviewFindingText{File: "x.go", Title: "alpha beta zeta eta"},
		),
	}

	got := RepeatedFindings(records)
	if got.Findings != 3 || got.Repeated != 1 {
		t.Fatalf("findings/repeated = %d/%d, want 3/1: three of five shared words reach the threshold", got.Findings, got.Repeated)
	}
}

// TestRepeatSimilarityThresholdPinned: the report's share is compared from run
// to run, so the threshold is a decision rather than a value to tune. Changing
// it means changing this test and the bead's calibration with it.
func TestRepeatSimilarityThresholdPinned(t *testing.T) {
	t.Parallel()
	if RepeatSimilarityThreshold != 0.6 {
		t.Fatalf("RepeatSimilarityThreshold = %v, want 0.6", RepeatSimilarityThreshold)
	}
}

// repeatLine marshals rec as one review-log line.
func repeatLine(t *testing.T, rec land.ReviewRecord) string {
	t.Helper()
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal %+v: %v", rec, err)
	}
	return string(line)
}

// TestReadRepeatReportCountsUnusableLines: an unreadable line, a line naming no
// bead and one carrying no instant are counted as skipped, so a log this reader
// cannot use is never an empty report; a queue line is feed bookkeeping and is
// not counted as either.
func TestReadRepeatReportCountsUnusableLines(t *testing.T) {
	t.Parallel()
	good := repeatLine(t, repeatRound("gt-a", 1, 20,
		land.ReviewFindingText{File: "a.go", Title: "one"}))
	// A queue line that names a finding count is the case a reader must not
	// take for a verdict: were it read as one, it would count in NoText.
	queue := repeatLine(t, land.ReviewRecord{
		BeadID: "gt-q", Rig: "gastown", Attempt: 1, Findings: 5,
		Queue: &land.FollowupQueue{Findings: []land.ReviewFindingText{{File: "q.go", Title: "held"}}},
		At:    time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	})
	log := strings.Join([]string{
		good,
		"{not json",
		queue,
		`{"attempt":1,"at":"2026-10-20T12:00:00Z"}`,
		`{"bead":"gt-z","attempt":1}`,
		"",
	}, "\n")

	got, err := ReadRepeatReport(strings.NewReader(log), time.Time{})
	if err != nil {
		t.Fatalf("ReadRepeatReport: %v", err)
	}
	if got.Skipped != 3 {
		t.Errorf("skipped = %d, want 3", got.Skipped)
	}
	if got.Findings != 1 || got.NoText != 0 {
		t.Errorf("findings/no_text = %d/%d, want 1/0", got.Findings, got.NoText)
	}
}

// TestReadRepeatReportSinceFiltersRounds: --since keeps only the rounds in
// range, so an earlier round outside it is not a baseline.
func TestReadRepeatReportSinceFiltersRounds(t *testing.T) {
	t.Parallel()
	earlier := land.ReviewFindingText{File: "a.go", Title: "alpha beta gamma delta"}
	restated := land.ReviewFindingText{File: "a.go", Title: "alpha beta gamma epsilon"}
	log := strings.Join([]string{
		repeatLine(t, repeatRound("gt-a", 1, 20, earlier)),
		repeatLine(t, repeatRound("gt-a", 2, 21, restated)),
		"",
	}, "\n")

	all, err := ReadRepeatReport(strings.NewReader(log), time.Time{})
	if err != nil {
		t.Fatalf("ReadRepeatReport: %v", err)
	}
	if all.Findings != 2 || all.Repeated != 1 {
		t.Fatalf("all rounds: findings/repeated = %d/%d, want 2/1", all.Findings, all.Repeated)
	}

	since := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	inRange, err := ReadRepeatReport(strings.NewReader(log), since)
	if err != nil {
		t.Fatalf("ReadRepeatReport since: %v", err)
	}
	if inRange.Findings != 1 || inRange.Repeated != 0 {
		t.Errorf("since 2026-10-21: findings/repeated = %d/%d, want 1/0", inRange.Findings, inRange.Repeated)
	}
}

// failingReader is an input that fails on its first read: the log a report must
// not take for an empty one.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk error") }

// TestReadRepeatReportUnreadableInputFails: a review log that cannot be read
// fails the report rather than yielding an empty one.
func TestReadRepeatReportUnreadableInputFails(t *testing.T) {
	t.Parallel()
	_, err := ReadRepeatReport(failingReader{}, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "reading review records") {
		t.Fatalf("error = %v, want a read failure", err)
	}
}
