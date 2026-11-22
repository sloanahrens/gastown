package land

import (
	"os"
	"strings"
	"testing"
)

// TestFollowupSkipsRecordSurvivesCompaction: a queued finding the skip policy
// drops is consumed from the queue, and the skips line of the pass that consumed
// it is the only record left: its class, file, reason and fingerprint, and no
// title or claim text (gt-0zqese).
func TestFollowupSkipsRecordSurvivesCompaction(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, nil) // nothing of its own to file
	f.lander.FollowupSkip = skipByTitle("policy", queuedFinding.Title)
	f.lander.FollowupClass = func(c FollowupCandidate) string { return "class:" + c.Path }
	if err := f.reviews.AppendQueue(queueLine("gt-queued", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.run()
	if queued, err := f.reviews.Queued(); err != nil || len(queued) != 0 {
		t.Errorf("queue = %+v (%v), want the skipped finding's line consumed", queued, err)
	}
	want := FollowupSkipped{Class: "class:" + queuedFinding.File, File: queuedFinding.File, Reason: "policy", Fingerprint: Fingerprint(queuedFinding.File, queuedFinding.Title)}
	raw, err := os.ReadFile(f.reviews.Path)
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		t.Fatalf("the reviews log holds no line after the pass (%v): the skipped queued finding left no trace", err)
	}
	got := skipsLines(t, f.reviews)
	if len(got) != 1 || len(got[0].Findings) != 1 || got[0].Findings[0] != want {
		t.Fatalf("skips lines = %+v, want one line holding %+v", got, want)
	}
	inspected := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(line, `"followup_skips"`) {
			continue
		}
		inspected++
		for _, text := range []string{queuedFinding.Title, queuedFinding.Claim} {
			if strings.Contains(line, text) {
				t.Errorf("the skips line holds the finding text %q", text)
			}
		}
	}
	if inspected != 1 {
		t.Errorf("inspected %d skips lines of the raw file, want 1", inspected)
	}
}
