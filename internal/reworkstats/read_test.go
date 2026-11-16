package reworkstats

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// writeReadFile writes body at dir/name for a reader test.
func writeReadFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadLandingsMissingDirectoryIsATownThatHasNotLanded: no directory is no
// records and no error, so a new town is not a failed read.
func TestReadLandingsMissingDirectoryIsATownThatHasNotLanded(t *testing.T) {
	t.Parallel()
	recs, bad, err := ReadLandings(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(recs) != 0 || bad != 0 {
		t.Fatalf("ReadLandings(absent) = %d records, %d bad, %v; want none and no error", len(recs), bad, err)
	}
}

// TestReadLandingsReadsLedgersAndSkipsTheReviewLogAndDirectories: every rig's
// .jsonl ledger counts, the review log in the same directory does not, and a
// line that is not a record is counted, not dropped silently.
func TestReadLandingsReadsLedgersAndSkipsTheReviewLogAndDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, dir, "gastown.jsonl", `{"bead":"gt-a","rig":"gastown","landed_at":"2026-11-02T12:00:00Z"}`+"\nnot json\n")
	writeReadFile(t, dir, "om.jsonl", `{"bead":"om-b","rig":"om","landed_at":"2026-11-03T12:00:00Z"}`+"\n")
	writeReadFile(t, dir, land.ReviewsFileName, `{"bead":"gt-zzz","rig":"gastown"}`+"\n")
	writeReadFile(t, dir, "notes.txt", "not a ledger\n")
	if err := os.Mkdir(filepath.Join(dir, "backoff.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}

	recs, bad, err := ReadLandings(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range recs {
		got[r.Bead] = true
	}
	if len(recs) != 2 || !got["gt-a"] || !got["om-b"] {
		t.Fatalf("records = %+v, want gt-a and om-b only", recs)
	}
	if bad != 1 {
		t.Errorf("bad lines = %d, want the one line that is not a record", bad)
	}
}

// TestReadReviewsSkipsQueueAndBlankLinesAndCountsTheUnreadable: a queue line and
// a blank line are not verdicts and not unreadable, and a line that is not JSON
// is counted so a lost verdict is never read as fewer rounds.
func TestReadReviewsSkipsQueueAndBlankLinesAndCountsTheUnreadable(t *testing.T) {
	t.Parallel()
	path := writeReadFile(t, t.TempDir(), land.ReviewsFileName,
		`{"bead":"gt-a","verdict":"approve"}`+"\n"+
			`{"followup_queue":{"n":1}}`+"\n\n"+
			"garbage\n")

	recs, unreadable, err := ReadReviews(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].BeadID != "gt-a" {
		t.Errorf("records = %+v, want the one verdict", recs)
	}
	if unreadable != 1 {
		t.Errorf("unreadable = %d, want only the garbage line", unreadable)
	}
}

// TestReadReviewsMissingLogIsATownNeverReviewed: no file is no records and no
// error.
func TestReadReviewsMissingLogIsATownNeverReviewed(t *testing.T) {
	t.Parallel()
	recs, unreadable, err := ReadReviews(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil || len(recs) != 0 || unreadable != 0 {
		t.Fatalf("ReadReviews(absent) = %d, %d, %v; want none and no error", len(recs), unreadable, err)
	}
}
