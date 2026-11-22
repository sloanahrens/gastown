package land

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestFollowupSkipsLineRoundTrips: AppendSkips writes one line that reads back
// to the record it was given, and the line holds no title or claim (gt-plcst6).
func TestFollowupSkipsLineRoundTrips(t *testing.T) {
	t.Parallel()
	f := reviewsFileIn(t)
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	rec := FollowupSkipsRecord{BeadID: "gt-plcst6", Rig: "gastown", At: at, Findings: []FollowupSkipped{
		{Class: "comment-claim", File: "note.go", Reason: "doc nit", Fingerprint: Fingerprint("note.go", "name the error")},
		{Class: "protected-path", File: "hook.go", Reason: "workflow", Fingerprint: Fingerprint("hook.go", "swap the nil check")},
	}}
	if err := f.AppendSkips(rec); err != nil {
		t.Fatalf("AppendSkips: %v", err)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got ReviewRecord
	if err := json.Unmarshal(raw, &got); err != nil || got.BeadID != rec.BeadID || got.Rig != rec.Rig ||
		!got.At.Equal(rec.At) || got.Skips == nil || !reflect.DeepEqual(got.Skips.Findings, rec.Findings) {
		t.Fatalf("the line reads back as %+v, %v; want %+v", got, err, rec)
	}
	if n := strings.Count(string(raw), "\n"); n != 1 {
		t.Errorf("AppendSkips wrote %d newlines, want one", n)
	}
	for _, field := range []string{`"title":`, `"claim":`} {
		if strings.Contains(string(raw), field) {
			t.Errorf("the skips line holds a %s field:\n%s", field, raw)
		}
	}
}

// TestFollowupSkipsLineIsNotAVerdict: the verdict reader counts the same
// landings and findings with and without a skips line, the queue reader does
// not read it, and IsQueueLine names it a non-verdict (gt-plcst6).
func TestFollowupSkipsLineIsNotAVerdict(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	verdict := ReviewRecord{BeadID: "gt-v", Rig: "gastown", Verdict: VerdictApprove, Findings: 2, At: at}
	plain, skipped := reviewsFileIn(t), reviewsFileIn(t)
	for _, f := range []*ReviewsFile{plain, skipped} {
		if err := f.Append(verdict); err != nil {
			t.Fatal(err)
		}
	}
	if err := skipped.AppendSkips(FollowupSkipsRecord{BeadID: "gt-skip", Rig: "gastown", At: at,
		Findings: []FollowupSkipped{{Class: "doc-nit", File: "a.go", Reason: "wording", Fingerprint: "om-fp:1"}}}); err != nil {
		t.Fatal(err)
	}
	before, err1 := plain.Since(time.Time{})
	after, err2 := skipped.Since(time.Time{})
	if err1 != nil || err2 != nil || len(before) != 1 || len(after) != 1 || after[0].Findings != before[0].Findings {
		t.Fatalf("Since = %d landings with the skips line, %d without (%v, %v)", len(after), len(before), err1, err2)
	}
	if queued, err := skipped.Queued(); err != nil || len(queued) != 0 {
		t.Errorf("Queued read the skips line as %+v, %v; want none", queued, err)
	}
	raw, err := os.ReadFile(skipped.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !IsQueueLine([]byte(strings.Split(strings.TrimSpace(string(raw)), "\n")[1])) {
		t.Errorf("IsQueueLine does not name the skips line a non-verdict:\n%s", raw)
	}
}
