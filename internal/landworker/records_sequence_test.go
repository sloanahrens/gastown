package landworker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// flapBeads is a beads fake with a bd outage: its first noteFails note writes
// and its next showFails reads fail.
type flapBeads struct {
	*beadsfake.Fake
	noteFails, showFails int
}

func (f *flapBeads) AppendNotes(id, note string) error {
	if f.noteFails == 0 {
		return f.Fake.AppendNotes(id, note)
	}
	f.noteFails--
	return errors.New("bd down")
}

func (f *flapBeads) Show(id string) (*beads.Issue, error) {
	if f.showFails == 0 {
		return f.Fake.Show(id)
	}
	f.showFails--
	return nil, errors.New("bd down")
}

// gateStub is the candidate, merger and reviewer a policy refusal never reaches.
type gateStub struct{}

func (gateStub) Run(context.Context, land.Repo, string, land.Work, string) land.CandidateResult {
	return land.CandidateResult{}
}
func (gateStub) Discard(context.Context, land.Work, land.CandidateResult) {}
func (gateStub) Merge(context.Context, land.MergeRequest) error           { return nil }
func (gateStub) Review(context.Context, string, string, string) (land.Verdict, error) {
	return land.Verdict{}, nil
}

// TestDrainRecordQueueSequence drives the note a rejected landing queued through
// the worker's later passes: order A's drain writes and drops it; order B's flap
// blinds the drain while the landing writes it, and the next pass drops it.
func TestDrainRecordQueueSequence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		flap bool     // the drain's read fails before pass 2
		want [][2]int // queued records and rejection blocks after each pass
	}{
		{"order A: the drain writes the note", false, [][2]int{{1, 0}, {0, 1}}},
		{"order B: the landing writes the note", true, [][2]int{{1, 0}, {1, 1}, {0, 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedReady(t, "gt-abc")
			issue, err := h.bd.Show("gt-abc")
			if err != nil {
				t.Fatal(err)
			}
			issue.AcceptanceCriteria = "- [ ] unchecked" // a policy refusal, before any git
			h.bd.Seed(*issue)
			bd := &flapBeads{Fake: h.bd, noteFails: 1}
			q := land.RecordQueue{Path: filepath.Join(queueDir(t), "records.jsonl")}
			landings, err := land.RigLandingsFile(t.TempDir(), "gastown")
			if err != nil {
				t.Fatal(err)
			}
			h.w.Lander = &land.Lander{Repo: "/nonexistent/repo", WorkRoot: "/nonexistent/work",
				Candidate: gateStub{}, Merger: gateStub{}, Reviewer: gateStub{},
				Beads: bd, Landings: landings, Records: &q, Now: func() time.Time { return h.now }}
			h.w.Beads, h.w.Records, h.w.LandTimeout = bd, &q, time.Second
			for i, want := range tc.want {
				if tc.flap && i == 1 { // the flap that blinds the drain's read
					bd.showFails = 1
				}
				if i > 0 {
					h.now = h.now.Add(10 * time.Minute) // past the queued record's backoff
				}
				h.w.Pass(context.Background())
				recs, err := q.Pending()
				got, gerr := h.bd.Show("gt-abc")
				if err != nil || gerr != nil {
					t.Fatalf("reading the queue and the bead: %v; %v", err, gerr)
				}
				if len(recs) != want[0] || strings.Count(got.Notes, land.MergeRejectionNoteMarker) != want[1] {
					t.Fatalf("pass %d: %d queued, notes %q; want %d, %d", i+1, len(recs), got.Notes, want[0], want[1])
				}
			}
		})
	}
}
