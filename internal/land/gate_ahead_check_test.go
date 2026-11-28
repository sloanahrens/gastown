package land

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// gateAheadReadyIssue is the bead a submission that may be gated ahead carries:
// actionable, labelled for landing, and with no policy refusal.
func gateAheadReadyIssue(id string) beads.Issue {
	return beads.Issue{ID: id, Title: "add b", Status: "hooked", Type: "task",
		Assignee: "gastown/polecats/opal", Labels: []string{LabelReadyToLand}}
}

// gateAheadNilShowBeads answers a read with neither a bead nor an error, the
// shape a client should never return but a check must not mistake for ready.
type gateAheadNilShowBeads struct{ Beads }

func (gateAheadNilShowBeads) Show(string) (*beads.Issue, error) { return nil, nil }

func TestGateAheadCheckBead(t *testing.T) {
	t.Parallel()
	const beadID = "gt-abc"
	rows := []struct {
		name string
		// beads is built per row: a fake store is journaled, so the "no bead
		// write" check can also see that the check wrote nothing.
		beads func() Beads
		// want is the text the returned error must name; empty means nil.
		want string
	}{
		{"ready", func() Beads {
			bd := beadsfake.New(beadsfake.WithPrefix("gt"))
			bd.Seed(gateAheadReadyIssue(beadID))
			return bd
		}, ""},
		{"unreadable bead", func() Beads {
			bd := beadsfake.New(beadsfake.WithPrefix("gt"))
			bd.FailWith("show", errors.New("bd show: database unavailable"))
			return bd
		}, "bd show: database unavailable"},
		{"no bead returned", func() Beads { return gateAheadNilShowBeads{} }, "no bead returned"},
		{"not actionable", func() Beads {
			i := gateAheadReadyIssue(beadID)
			i.Status = "closed"
			bd := beadsfake.New(beadsfake.WithPrefix("gt"))
			bd.Seed(i)
			return bd
		}, "is closed"},
		{"no ready label", func() Beads {
			i := gateAheadReadyIssue(beadID)
			i.Labels = nil
			bd := beadsfake.New(beadsfake.WithPrefix("gt"))
			bd.Seed(i)
			return bd
		}, "has no " + LabelReadyToLand + " label"},
		{"policy rejection", func() Beads {
			i := gateAheadReadyIssue(beadID)
			i.AcceptanceCriteria = "- [ ] add b"
			bd := beadsfake.New(beadsfake.WithPrefix("gt"))
			bd.Seed(i)
			return bd
		}, "unchecked acceptance criteria"},
		// Land runs the readiness checks before policy, so a bead failing
		// three of them is refused for the first: the order is what keeps a
		// gated-ahead bead one the landing would run, not skip.
		{"first reason", func() Beads {
			i := gateAheadReadyIssue(beadID)
			i.Status = "closed"
			i.Labels = nil
			i.AcceptanceCriteria = "- [ ] add b"
			bd := beadsfake.New(beadsfake.WithPrefix("gt"))
			bd.Seed(i)
			return bd
		}, "is closed"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := tc.beads()
			l := &Lander{Beads: store}
			var before int
			journaled, ok := store.(*beadsfake.Fake)
			if ok {
				before = aheadBeadWrites(t, journaled)
			}
			err := l.gateAheadBeadCheck(Work{BeadID: beadID})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("gateAheadBeadCheck = %v, want nil", err)
				}
			} else if err == nil {
				t.Fatalf("gateAheadBeadCheck = nil, want an error naming %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("gateAheadBeadCheck error %q does not name %q", err, tc.want)
			}
			if ok {
				if n := aheadBeadWrites(t, journaled) - before; n != 0 {
					t.Errorf("gateAheadBeadCheck wrote %d bead records, want 0", n)
				}
			}
		})
	}
}
