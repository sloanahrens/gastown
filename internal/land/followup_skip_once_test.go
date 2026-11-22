package land

import (
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// scriptedSkip is a stateful FollowupSkip: it answers each finding from a script
// by the number of times it has been asked, and counts the asks per finding.
type scriptedSkip struct {
	mu    sync.Mutex
	calls map[string]int
	reply func(title string, nth int) string
}

func (s *scriptedSkip) ask(_ Work, c FollowupCandidate) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[c.Title]++
	return s.reply(c.Title, s.calls[c.Title])
}

// TestFollowupSkipIsCalledOncePerFindingPerPass: a queued finding is judged by the
// skip hook once in a feed pass, however many times the drain and the compaction
// read its queue line, so a hook that answers differently on a second ask cannot
// leave the line half consumed (gt-r66hk7, om finding on gt-8fdomo). The hook skips
// a finding the first time and would keep it every time after; the line is skipped,
// consumed and dropped from the log, and the hook was asked once.
func TestFollowupSkipIsCalledOncePerFindingPerPass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		reply       func(string, int) string
		wantCreated int
	}{
		{"skip once, then keep", func(_ string, n int) string {
			if n == 1 {
				return "wording nit (Q34)"
			}
			return ""
		}, 0},
		{"keep once, then skip", func(_ string, n int) string {
			if n == 1 {
				return ""
			}
			return "wording nit (Q34)"
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.9, nil)
			if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
				t.Fatal(err)
			}
			f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
			f.fake.existing = openFollowupBeads(1)
			skip := &scriptedSkip{reply: tc.reply}
			f.lander.FollowupSkip = skip.ask
			log := f.run()
			if got := skip.calls["name the error"]; got != 1 {
				t.Errorf("the hook was asked %d times about the finding, want once:\n%s", got, log)
			}
			if len(f.fake.created) != tc.wantCreated {
				t.Errorf("created %d beads, want %d:\n%s", len(f.fake.created), tc.wantCreated, log)
			}
			if queued, err := f.reviews.Queued(); err != nil || len(queued) != 0 {
				t.Errorf("queue = %+v (%v), want the consumed line dropped", queued, err)
			}
		})
	}
}

// TestFollowupSkipAnswersAreKeptPerBead: the same finding on two queue lines of
// different landings is judged once for each, because the hook is given the
// landing it came from: here it skips the finding for gt-held-a and keeps it for
// gt-held-b, so one bead is filed and each landing was asked once (gt-r66hk7).
func TestFollowupSkipAnswersAreKeptPerBead(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil)
	for _, bead := range []string{"gt-held-a", "gt-held-b"} {
		if err := f.reviews.AppendQueue(queueLine(bead, f.work.Rig, queuedFinding)); err != nil {
			t.Fatal(err)
		}
		f.bd.Seed(beads.Issue{ID: bead, Title: "an earlier landing", Status: "closed", Type: "bug"})
	}
	asked := map[string]int{}
	f.lander.FollowupSkip = func(w Work, _ FollowupCandidate) string {
		asked[w.BeadID]++
		if w.BeadID == "gt-held-a" {
			return "wording nit (Q34)"
		}
		return ""
	}
	f.fake.existing = openFollowupBeads(1)
	log := f.run()
	if asked["gt-held-a"] != 1 || asked["gt-held-b"] != 1 {
		t.Errorf("asked %v, want each landing asked once:\n%s", asked, log)
	}
	if len(f.fake.created) != 1 {
		t.Errorf("created %d beads, want one for the landing whose finding was kept:\n%s", len(f.fake.created), log)
	}
}
