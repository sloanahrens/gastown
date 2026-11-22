package land

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestFollowupSkipsLineRoundTrips: AppendSkips writes one line, reads back, holds no title or claim, and refuses an oversize line (gt-plcst6).
func TestFollowupSkipsLineRoundTrips(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	findings := []FollowupSkipped{{Class: "comment-claim", File: "note.go", Reason: "doc nit", Fingerprint: Fingerprint("note.go", "name the error")}}
	f := &ReviewsFile{Path: privateLandingsPath(t)}
	must(t, f.AppendSkips(FollowupSkipsRecord{BeadID: "gt-plcst6", Rig: "gastown", At: at, Findings: findings}))
	want, err := json.Marshal(ReviewRecord{BeadID: "gt-plcst6", Rig: "gastown", At: at, Skips: &FollowupSkips{Findings: findings}})
	must(t, err)
	raw, err := os.ReadFile(f.Path)
	must(t, err)
	if string(raw) != string(want)+"\n" || strings.Contains(string(raw), `"title"`) || strings.Contains(string(raw), `"claim"`) {
		t.Fatalf("AppendSkips wrote %q, want one line %q with no title or claim", raw, want)
	}
	if f.AppendSkips(FollowupSkipsRecord{BeadID: "gt-big", Findings: []FollowupSkipped{{Reason: strings.Repeat("x", maxReviewLineBytes)}}}) == nil {
		t.Error("AppendSkips accepted a record past the line bound")
	}
}

// TestFollowupSkipsLineIsNotAVerdict: the verdict readers count the same landings and findings with and without a skips line (gt-plcst6).
func TestFollowupSkipsLineIsNotAVerdict(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	f := &ReviewsFile{Path: privateLandingsPath(t)}
	must(t, f.Append(ReviewRecord{BeadID: "gt-v", Rig: "gastown", Verdict: VerdictApprove, Findings: 2, At: at}))
	before, err := f.Since(time.Time{})
	must(t, err)
	must(t, f.AppendSkips(FollowupSkipsRecord{BeadID: "gt-s", Rig: "gastown", At: at, Findings: []FollowupSkipped{{Class: "doc-nit"}}}))
	after, err := f.Since(time.Time{})
	must(t, err)
	queued, err := f.Queued()
	if err != nil || len(before) != 1 || len(after) != 1 || after[0].Findings != before[0].Findings || after[0].Verdict != before[0].Verdict || len(queued) != 0 {
		t.Fatalf("Since = %+v then %+v, queued %+v (%v); want the skips line skipped everywhere", before, after, queued, err)
	}
}
