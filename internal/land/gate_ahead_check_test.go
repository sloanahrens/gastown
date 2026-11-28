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

// TestGateAheadCheck: a submission passing the bead, head and range checks is
// accepted. The first check to fail returns an error naming it, the checks
// after it do not run, and no check writes to the bead.
func TestGateAheadCheck(t *testing.T) {
	t.Parallel()
	boom := errors.New("git read failed")
	passing := func(CommitReader, string, string) (*Rejection, error) { return nil, nil }
	erroring := func(CommitReader, string, string) (*Rejection, error) { return nil, boom }
	rejecting := func(CommitReader, string, string) (*Rejection, error) {
		return &Rejection{Kind: RejectPolicy, Reason: "a forbidden trailer"}, nil
	}
	tests := []struct {
		name string
		bead func(f *landFixture)
		head func(t *testing.T, f *landFixture, r *gateAheadHeadRepo)
		// ranges are the lander's RangeChecks. The bead and head rows carry
		// rejecting, so a check that ran past its failure would surface as
		// "range check 0" and fail the row's wantErr.
		ranges  []RangeCheck
		wantErr string
	}{
		{name: "all three pass", ranges: []RangeCheck{passing}},
		{name: "bead check error", bead: func(f *landFixture) { f.bd.FailWith("show", boom) },
			ranges: []RangeCheck{rejecting}, wantErr: "reading bead gt-abc"},
		{name: "bead not ready", bead: func(f *landFixture) {
			i := gateAheadReadyIssue("gt-abc")
			i.Labels = nil
			f.bd.Seed(i)
		}, ranges: []RangeCheck{rejecting}, wantErr: "has no " + LabelReadyToLand + " label"},
		{name: "head check error", head: func(_ *testing.T, _ *landFixture, r *gateAheadHeadRepo) { r.branchFetchErr = boom },
			ranges: []RangeCheck{rejecting}, wantErr: "fetch branch"},
		{name: "head not on the branch", head: func(t *testing.T, f *landFixture, _ *gateAheadHeadRepo) {
			f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
		}, ranges: []RangeCheck{rejecting}, wantErr: "not reachable"},
		{name: "range check error", ranges: []RangeCheck{erroring},
			wantErr: "landing failed at range check: git read failed"},
		{name: "range check rejects", ranges: []RangeCheck{passing, rejecting},
			wantErr: "range check 1: landing rejected (policy): a forbidden trailer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			if tt.bead != nil {
				tt.bead(f)
			}
			l := f.lander()
			l.RangeChecks = tt.ranges
			r := &gateAheadHeadRepo{Repo: f.git.Open(f.repo)}
			if tt.head != nil {
				tt.head(t, f, r)
			}
			before := aheadBeadWrites(t, f.bd)
			err := l.gateAheadCheck(r, f.work, f.base)
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("gateAheadCheck wrote %d bead records, want 0", n)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("gateAheadCheck = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("gateAheadCheck = %v, want an error naming %q", err, tt.wantErr)
			}
		})
	}
}

// TestGateAheadCheckRangesReadBaseToHead: the range checks read the commits
// the landing would bring on, base..head.
func TestGateAheadCheckRangesReadBaseToHead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	var gotBase, gotHead string
	l.RangeChecks = []RangeCheck{func(_ CommitReader, base, head string) (*Rejection, error) {
		gotBase, gotHead = base, head
		return nil, nil
	}}
	if err := l.gateAheadCheck(f.git.Open(f.repo), f.work, f.base); err != nil {
		t.Fatalf("gateAheadCheck = %v, want nil", err)
	}
	if gotBase != f.base || gotHead != f.work.Head {
		t.Errorf("range check read %s..%s, want %s..%s", gotBase, gotHead, f.base, f.work.Head)
	}
}
