package land

import (
	"slices"
	"strings"
	"testing"
)

// skipByTitle is a FollowupSkip that drops the named findings with reason, the
// way the daemon's policy will (gt-3v8jno).
func skipByTitle(reason string, titles ...string) func(Work, FollowupCandidate) string {
	return func(_ Work, c FollowupCandidate) string {
		if slices.Contains(titles, c.Title) {
			return reason
		}
		return ""
	}
}

// TestFollowupSkipDropsTheNamedFinding: a finding FollowupSkip names a reason
// for is dropped before it reaches the bead, and the feed logs it (gt-3v8jno).
func TestFollowupSkipDropsTheNamedFinding(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorAt("internal/land/note.go", 230, "wording nit"),
		minorFinding("name the error"),
	})
	f.lander.FollowupSkip = skipByTitle("wording nit (Q34)", "wording nit")
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one for the finding the policy kept:\n%s", len(f.fake.created), log)
	}
	want := []string{FollowUpLabel, Fingerprint("internal/land/note.go", "name the error")}
	if got := f.fake.created[0].Labels; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want only the kept finding's fingerprint", got)
	}
	if strings.Contains(f.fake.created[0].Body, "wording nit") {
		t.Errorf("the bead body names the skipped finding:\n%s", f.fake.created[0].Body)
	}
	if !strings.Contains(log, "skipped 1 finding: wording nit (Q34)") {
		t.Errorf("log = %q, want the skip and its reason named", log)
	}
}

// TestFollowupSkipIsNeverQueuedOrCommented: a skipped finding is not work the
// feed holds for later, and it is not a repeat of a bead that carries it — the
// policy is asked before either decision (gt-3v8jno).
func TestFollowupSkipIsNeverQueuedOrCommented(t *testing.T) {
	t.Parallel()
	t.Run("not queued at the backpressure bound", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{
			minorAt("internal/land/note.go", 230, "wording nit a"),
			minorAt("internal/land/note.go", 231, "wording nit b"),
			minorFinding("name the error"),
		})
		f.lander.FollowupSkip = skipByTitle("wording nit (Q34)", "wording nit a", "wording nit b")
		f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads at the backpressure bound", len(f.fake.created))
		}
		queued, err := f.reviews.Queued()
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) != 1 || len(queued[0].Findings) != 1 || queued[0].Findings[0].Title != "name the error" {
			t.Errorf("queue lines = %+v, want one line holding only the finding the policy kept", queued)
		}
		// The line counts both skipped findings and names the reason once.
		if !strings.Contains(log, "skipped 2 findings: wording nit (Q34)") || strings.Count(log, "wording nit (Q34)") != 1 {
			t.Errorf("log = %q, want both findings counted and the reason once", log)
		}
	})
	t.Run("not commented onto the bead that carries it", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "wording nit")})
		f.lander.FollowupSkip = skipByTitle("wording nit (Q34)", "wording nit")
		f.fake.existing = []ExistingFollowup{{
			ID: "gt-old", Title: "wording nit", Status: "open",
			Labels: []string{FollowUpLabel, Fingerprint("internal/land/note.go", "wording nit")},
		}}
		f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads for a skipped finding", len(f.fake.created))
		}
		comments, err := f.bd.Comments("gt-old")
		if err != nil {
			t.Fatal(err)
		}
		if len(comments) != 0 {
			t.Errorf("comments = %v, want none for a skipped finding", commentTexts(comments))
		}
	})
}

// TestFollowupSkipDropsAQueuedFindingItNames: the hook reaches the queue too, so
// a queued finding it skips is not filed and its line is consumed rather than
// waiting forever (gt-3v8jno).
func TestFollowupSkipDropsAQueuedFindingItNames(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, nil)
	f.lander.FollowupSkip = skipByTitle("wording nit (Q34)", "name the error")
	if err := f.reviews.AppendQueue(queueLine("gt-queued", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads for a queued finding the policy skipped:\n%s", len(f.fake.created), log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("the log still holds %d queue lines, want the skipped one consumed", len(queued))
	}
	if !strings.Contains(log, "skipped 1 finding: wording nit (Q34)") {
		t.Errorf("log = %q, want the queued finding's skip named", log)
	}
}
