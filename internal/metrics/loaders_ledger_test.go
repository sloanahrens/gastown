package metrics

import (
	"reflect"
	"testing"
)

// These fixtures are the Python loader tests' fixtures, line for line
// (scripts/test_town_metrics_loaders.py), so a ported case has the same inputs
// and expected values as the case it replaces.

var ledgerFixture = []string{
	`{"bead": "gt-1", "rig": "gastown", "landed_commit": "abc", "landed_at": "2026-10-01T00:00:00Z", "route": "daemon", "om_verdict": "approve", "gate_result": "build+lint"}`,
	"",
	"{not json",
	`{"rig": "gastown", "landed_at": "2026-10-01T00:00:00Z"}`, // no bead
}

var reviewFixture = []string{
	`{"bead": "gt-1", "rig": "gastown", "attempt": 1, "score": 0.6, "verdict": "request_changes", "at": "2026-09-30T23:00:00Z"}`,
	`{"bead": "gt-1", "rig": "gastown", "attempt": 2, "score": 0.9, "verdict": "approve", "at": "2026-09-30T23:30:00Z"}`,
	`{"bead": "gt-2", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-02T00:00:00Z"}`,
	`{"bead": "gt-3", "rig": "gastown", "attempt": 1, "score": 0.5, "verdict": "request_changes", "at": "2026-10-05T00:00:00Z"}`,
	`{"bead": "gt-5", "rig": "gastown", "attempt": 1, "score": 0.5, "verdict": "request_changes", "at": "2026-10-06T00:00:00Z"}`,
}

func TestLoadLedgerRowsAndDropsAccountForEveryNonblankLine(t *testing.T) {
	t.Parallel()
	var d Dropped
	rows := LoadLedger(map[string][]string{"gastown": ledgerFixture}, &d)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one row", rows)
	}
	want := LedgerRow{
		Bead: "gt-1", Rig: "gastown", LandedCommit: "abc", LandedAt: "2026-10-01T00:00:00Z",
		Route: "daemon", OMVerdict: "approve", GateResult: "build+lint",
	}
	if rows[0] != want {
		t.Errorf("row = %+v, want %+v", rows[0], want)
	}
	if d.Count() != 2 {
		t.Errorf("drops = %d, want 2", d.Count())
	}
	if len(rows)+d.Count() != 3 { // 3 non-blank lines
		t.Errorf("rows + drops = %d, want 3", len(rows)+d.Count())
	}
}

func TestLoadLedgerTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lines   []string
		want    []LedgerRow
		dropped []string
	}{
		{
			name:  "the source rig fills a line that names none",
			lines: []string{`{"bead": "gt-2", "landed_at": "2026-10-02T00:00:00Z"}`},
			want:  []LedgerRow{{Bead: "gt-2", Rig: "gastown", LandedAt: "2026-10-02T00:00:00Z"}},
		},
		{
			name:  "a blank line is not a row and not a drop",
			lines: []string{"  \t", `{"bead": "gt-3", "landed_at": "2026-10-03T00:00:00Z"}`},
			want:  []LedgerRow{{Bead: "gt-3", Rig: "gastown", LandedAt: "2026-10-03T00:00:00Z"}},
		},
		{
			name:    "JSON that is not an object names no bead",
			lines:   []string{`["gt-4"]`},
			dropped: []string{"missing bead or landed_at"},
		},
		{
			name:    "a line that is not JSON is unparseable",
			lines:   []string{"{oops"},
			dropped: []string{"unparseable json"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var d Dropped
			got := LoadLedger(map[string][]string{"gastown": tc.lines}, &d)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rows = %+v, want %+v", got, tc.want)
			}
			var reasons []string
			for _, item := range d.Items {
				reasons = append(reasons, item.Reason)
			}
			if !reflect.DeepEqual(reasons, tc.dropped) {
				t.Errorf("drop reasons = %v, want %v", reasons, tc.dropped)
			}
		})
	}
}

func TestReworkByBead(t *testing.T) {
	t.Parallel()
	var d Dropped
	rv := LoadReviews(reviewFixture, &d)
	landed := map[string]string{
		"gt-1": "2026-10-01T00:00:00Z", "gt-2": "2026-10-02T01:00:00Z", "gt-4": "2026-10-03T00:00:00Z",
		"gt-5": "2026-10-05T00:00:00Z",
	}
	got, err := ReworkByBead(rv, landed, nil)
	if err != nil {
		t.Fatalf("ReworkByBead: %v", err)
	}
	want := map[string]Rework{
		"gt-1": {Attempts: 2, RejectedBeforeLanding: true},
		"gt-2": {Attempts: 1, RejectedBeforeLanding: false},
		"gt-4": {Attempts: 1, RejectedBeforeLanding: false}, // no review rows (om skipped)
		"gt-5": {Attempts: 1, RejectedBeforeLanding: false}, // a rejection after the landing is not before it
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReworkByBead = %+v, want %+v", got, want)
	}
	if _, ok := got["gt-3"]; ok {
		t.Errorf("gt-3 is in the result, want it absent: it never landed")
	}
}

func TestReworkByBeadNoteRejectionsCoverHistoryBeforeTheReviewLog(t *testing.T) {
	t.Parallel()
	landed := map[string]string{"gt-7": "2026-09-20T00:00:00Z", "gt-8": "2026-09-20T00:00:00Z"}
	got, err := ReworkByBead(nil, landed, map[string][]string{"gt-7": {"gate", "review"}})
	if err != nil {
		t.Fatalf("ReworkByBead: %v", err)
	}
	want := map[string]Rework{
		"gt-7": {Attempts: 3, RejectedBeforeLanding: true},
		"gt-8": {Attempts: 1, RejectedBeforeLanding: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReworkByBead = %+v, want %+v", got, want)
	}
}

func TestReworkByBeadAttemptsIsTheLargerOfReviewsAndNotes(t *testing.T) {
	t.Parallel()
	var d Dropped
	rv := LoadReviews(reviewFixture[:2], &d) // gt-1 reaches attempt 2
	got, err := ReworkByBead(rv, map[string]string{"gt-1": "2026-10-01T00:00:00Z"}, map[string][]string{"gt-1": {"gate"}})
	if err != nil {
		t.Fatalf("ReworkByBead: %v", err)
	}
	if got["gt-1"].Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got["gt-1"].Attempts)
	}
}

func TestLoadReviewsBadRowIsDroppedNotZeroed(t *testing.T) {
	t.Parallel()
	var d Dropped
	rows := LoadReviews([]string{`{"bead": "x"}`, "nope"}, &d)
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
	if d.Count() != 2 {
		t.Errorf("drops = %d, want 2", d.Count())
	}
}

func TestLoadReviewsTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lines   []string
		want    []Review
		dropped []string
	}{
		{
			name:  "a complete row is read with its attempt and score",
			lines: []string{`{"bead": "gt-1", "rig": "gastown", "attempt": 2, "score": 0.9, "verdict": "approve", "at": "2026-09-30T23:30:00Z"}`},
			want:  []Review{{Bead: "gt-1", Rig: "gastown", Attempt: 2, Score: 0.9, Verdict: "approve", At: "2026-09-30T23:30:00Z"}},
		},
		{
			name:  "a missing rig reads as the empty rig, as the Python's dict.get",
			lines: []string{`{"bead": "gt-2", "attempt": 1, "score": 0.5, "verdict": "request_changes", "at": "2026-10-05T00:00:00Z"}`},
			want:  []Review{{Bead: "gt-2", Attempt: 1, Score: 0.5, Verdict: "request_changes", At: "2026-10-05T00:00:00Z"}},
		},
		{
			name:    "a row missing a required field is dropped",
			lines:   []string{`{"bead": "gt-3", "attempt": 1, "score": 0.5, "verdict": "approve"}`},
			dropped: []string{"missing field"},
		},
		{
			// The feed's own bookkeeping line carries no verdict and is not a
			// row the reader lost (gt-3arxj).
			name: "a follow-up queue line is skipped uncounted",
			lines: []string{
				`{"bead": "gt-4", "attempt": 1, "score": 0.7, "verdict": "approve", "at": "2026-10-07T00:00:00Z"}`,
				`{"bead": "gt-4", "rig": "gastown", "at": "2026-10-07T00:00:01Z", "followup_queue": {"findings": [{"file": "a.go", "title": "name it"}]}}`,
			},
			want: []Review{{Bead: "gt-4", Attempt: 1, Score: 0.7, Verdict: "approve", At: "2026-10-07T00:00:00Z"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var d Dropped
			got := LoadReviews(tc.lines, &d)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rows = %+v, want %+v", got, tc.want)
			}
			var reasons []string
			for _, item := range d.Items {
				reasons = append(reasons, item.Reason)
			}
			if !reflect.DeepEqual(reasons, tc.dropped) {
				t.Errorf("drop reasons = %v, want %v", reasons, tc.dropped)
			}
		})
	}
}

func TestReworkByBeadRejectsAnUnparseableTimestamp(t *testing.T) {
	t.Parallel()
	reviews := []Review{{Bead: "gt-1", Attempt: 1, Verdict: "request_changes", At: "not a date"}}
	if _, err := ReworkByBead(reviews, map[string]string{"gt-1": "2026-10-01T00:00:00Z"}, nil); err == nil {
		t.Error("ReworkByBead accepted a review at an unparseable time, want an error")
	}
	if _, err := ReworkByBead(reviews, map[string]string{"gt-1": "not a date"}, nil); err == nil {
		t.Error("ReworkByBead accepted a landing at an unparseable time, want an error")
	}
}
