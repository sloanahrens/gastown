package land

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestFollowupSkipCountsWindow: SkipCounts keys each skipped finding by
// reason/class, counts a repeated fingerprint once per bead and only inside the
// window, and leaves verdict and queue lines out (gt-aknjq7).
func TestFollowupSkipCountsWindow(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	repeat := Fingerprint("note.go", "name the error")
	docNit := func(fp string) FollowupSkipped {
		return FollowupSkipped{Reason: "doc-nit", Class: "comment-claim", Fingerprint: fp}
	}
	cases := []struct {
		name  string
		skips []FollowupSkipsRecord
		want  map[string]int
	}{
		{"repeat once per bead, only inside the window", []FollowupSkipsRecord{
			{BeadID: "gt-a", At: since.Add(-time.Hour), Findings: []FollowupSkipped{docNit(repeat), docNit("before-only")}},
			// gt-a repeats it on two landings at or after the cutoff, with another finding.
			{BeadID: "gt-a", At: since, Findings: []FollowupSkipped{docNit(repeat), docNit("another")}},
			{BeadID: "gt-a", At: since.Add(time.Hour), Findings: []FollowupSkipped{docNit(repeat)}},
			// Another bead counts the same fingerprint again, under its own key.
			{BeadID: "gt-b", At: since, Findings: []FollowupSkipped{docNit(repeat), {Reason: "protected", Class: "workflow", Fingerprint: "hook"}}},
		}, map[string]int{"doc-nit/comment-claim": 3, "protected/workflow": 1}},
		{"a finding with no fingerprint has none to count once", []FollowupSkipsRecord{
			{BeadID: "gt-x", At: since, Findings: []FollowupSkipped{docNit(""), docNit("")}},
		}, map[string]int{"doc-nit/comment-claim": 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := reviewsFileIn(t)
			for _, rec := range c.skips {
				must(t, f.AppendSkips(rec))
			}
			// Neither a verdict nor a queue line is a skips line.
			must(t, f.Append(ReviewRecord{BeadID: "gt-v", At: since, Verdict: VerdictApprove}))
			must(t, f.AppendQueue(QueuedFollowup{Bead: "gt-v", At: since, Findings: []ReviewFindingText{{Title: "held"}}}))
			got, err := f.SkipCounts(since)
			if err != nil {
				t.Fatalf("SkipCounts: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("SkipCounts = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFollowupSkipCountsNilAndMalformed: a nil reviews file is an error naming
// it, and a line the reader cannot parse is counted in no key without an error
// while the lines around it still count (gt-aknjq7).
func TestFollowupSkipCountsNilAndMalformed(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	var nilFile *ReviewsFile
	if _, err := nilFile.SkipCounts(since); err == nil || !strings.Contains(err.Error(), "reviews file is nil") {
		t.Errorf("SkipCounts on a nil file = %v, want an error naming the reviews file", err)
	}
	f := reviewsFileIn(t)
	if got, err := f.SkipCounts(since); err != nil || len(got) != 0 {
		t.Errorf("SkipCounts on a log that does not exist = %v, %v; want no counts and no error", got, err)
	}
	// The body parses up to its second finding: an error the reader ignores would
	// count the first as if the line were whole. The good lines around it count.
	must(t, os.MkdirAll(filepath.Dir(f.Path), 0o700))
	must(t, os.WriteFile(f.Path, []byte(`{"bead_id":"gt-bad","at":"2026-11-16T09:00:00Z","followup_skips":{"findings":[{"reason":"partial","class":"unreadable"},5]}}`+"\n"), 0o600))
	must(t, f.AppendSkips(FollowupSkipsRecord{BeadID: "gt-a", At: since, Findings: []FollowupSkipped{{Reason: "doc-nit", Class: "comment-claim", Fingerprint: "fp-before"}}}))
	must(t, f.AppendSkips(FollowupSkipsRecord{BeadID: "gt-b", At: since, Findings: []FollowupSkipped{{Reason: "doc-nit", Class: "comment-claim", Fingerprint: "fp-after"}}}))
	got, err := f.SkipCounts(since)
	if err != nil {
		t.Fatalf("SkipCounts with a malformed line: %v", err)
	}
	if want := map[string]int{"doc-nit/comment-claim": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("SkipCounts = %v, want %v: the malformed line was counted or stopped the read", got, want)
	}
	dir := reviewsFileIn(t)
	must(t, dir.AppendSkips(FollowupSkipsRecord{BeadID: "gt-d", At: since, Findings: []FollowupSkipped{{Reason: "doc-nit", Class: "comment-claim", Fingerprint: "fp-dir"}}}))
	dir.Path = filepath.Dir(dir.Path)
	if _, err := dir.SkipCounts(since); err == nil {
		t.Error("SkipCounts on a directory = nil, want the read error")
	}
	// A path under a file cannot be opened: the open error is not "no such file".
	base := filepath.Join(filepath.Dir(f.Path), "base")
	must(t, os.WriteFile(base, nil, 0o600))
	if _, err := (&ReviewsFile{Path: filepath.Join(base, ReviewsFileName)}).SkipCounts(since); err == nil || !strings.Contains(err.Error(), "opening reviews file") {
		t.Errorf("SkipCounts under a file = %v, want the open error", err)
	}
}

// TestFollowupSkipCountsLinePastCap: a skips line past maxReviewLineBytes is
// not counted and raises no error, while the valid lines before and after it
// are still counted (gt-9ijxjl).
func TestFollowupSkipCountsLinePastCap(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	f := reviewsFileIn(t)
	must(t, os.MkdirAll(filepath.Dir(f.Path), 0o700))
	// AppendSkips refuses an over-cap line, so the file is written by hand.
	before, err := json.Marshal(ReviewRecord{BeadID: "gt-a", At: since, Skips: &FollowupSkips{
		Findings: []FollowupSkipped{{Reason: "doc-nit", Class: "comment-claim", Fingerprint: "fp-before"}}}})
	must(t, err)
	over, err := json.Marshal(ReviewRecord{BeadID: "gt-over", At: since, Skips: &FollowupSkips{
		Findings: []FollowupSkipped{{Reason: "doc-nit", Class: "comment-claim", Fingerprint: "fp-over", File: strings.Repeat("x", maxReviewLineBytes)}}}})
	must(t, err)
	after, err := json.Marshal(ReviewRecord{BeadID: "gt-b", At: since, Skips: &FollowupSkips{
		Findings: []FollowupSkipped{{Reason: "protected", Class: "workflow", Fingerprint: "fp-after"}}}})
	must(t, err)
	must(t, os.WriteFile(f.Path, []byte(string(before)+"\n"+string(over)+"\n"+string(after)+"\n"), 0o600))
	got, err := f.SkipCounts(since)
	if err != nil {
		t.Fatalf("SkipCounts with an over-cap skips line: %v", err)
	}
	want := map[string]int{"doc-nit/comment-claim": 1, "protected/workflow": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SkipCounts = %v, want %v: the over-cap line was counted or the line after it was not", got, want)
	}
}
