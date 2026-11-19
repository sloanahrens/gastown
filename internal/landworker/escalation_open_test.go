package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestPassReEscalatesAfterClose: a capped loop whose lane escalation the
// overseer closed raises one on the next rejection of that lane, and none
// while the seam reads it open; a first rejection raises without asking the
// seam. A check that errors, or no seam at all, raises again and says so at
// every check, since a silenced alarm is the defect (gt-fjzstx, gt-il3out).
func TestPassReEscalatesAfterClose(t *testing.T) {
	t.Parallel()
	closed := func(string, string) (bool, error) { return false, nil }
	for _, tc := range []struct {
		name            string
		rej             land.Rejection
		seam            func(string, string) (bool, error)
		want, wantAsked int32
		beads, wantSaid int
	}{
		{"first rejection raises without asking", land.Rejection{Kind: land.RejectPolicy, Reason: "r"}, closed, 1, 0, 1, 0},
		{"closed escalation raises again", land.Rejection{Kind: land.RejectReview, Reason: "r", NoEscalate: true, PriorEscalated: true}, closed, 1, 1, 1, 0},
		{"open escalation stays silent", land.Rejection{Kind: land.RejectReview, Reason: "r", NoEscalate: true, PriorEscalated: true}, func(string, string) (bool, error) { return true, nil }, 0, 1, 1, 0},
		{"a seam error raises and says so every time", land.Rejection{Kind: land.RejectReview, Reason: "r", NoEscalate: true, PriorEscalated: true}, func(string, string) (bool, error) { return false, errors.New("store down") }, 2, 2, 2, 2},
		{"no seam raises and says so every time", land.Rejection{Kind: land.RejectReview, Reason: "r", NoEscalate: true, PriorEscalated: true}, nil, 2, 0, 2, 2},
		{"a lane that never escalated stays silent", land.Rejection{Kind: land.RejectReview, Reason: "r", NoEscalate: true}, closed, 0, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			var logs []string
			h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			for i := 0; i < tc.beads; i++ {
				h.seedReady(t, fmt.Sprintf("gt-abc%d", i))
			}
			var escalations, asked atomic.Int32
			h.w.Escalate = func(string, string, string) { escalations.Add(1) }
			if tc.seam != nil {
				h.w.EscalationOpen = func(beadID, lane string) (bool, error) {
					asked.Add(1)
					if lane != "polecat" {
						t.Errorf("asked about lane %q for %s; want the polecat lane", lane, beadID)
					}
					return tc.seam(beadID, lane)
				}
			}
			rej := tc.rej
			h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, &rej }
			h.w.Pass(context.Background())
			h.w.escWG.Wait()
			if n := escalations.Load(); n != tc.want {
				t.Errorf("escalated %d times; want %d", n, tc.want)
			}
			if n := asked.Load(); n != tc.wantAsked {
				t.Errorf("asked the seam %d times; want %d", n, tc.wantAsked)
			}
			if said := strings.Count(strings.Join(logs, "\n"), "raising it again"); said != tc.wantSaid {
				t.Errorf("doubt logged %d times; want %d", said, tc.wantSaid)
			}
		})
	}
}
