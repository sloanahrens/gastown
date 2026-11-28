package land

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// approvalFixture is a merge of the fixture's head with a stored approval of it.
type approvalFixture struct {
	f      *landFixture
	l      *Lander
	g      Repo
	dir    string
	merged string
	store  *AheadStore
	key    ReviewKey
}

// put stores a review of fx's merge, dated at, with verdict v when it is set.
func (fx *approvalFixture) put(t *testing.T, at time.Time, v *Verdict) {
	rec := reuseAskRecord(fx.key, fx.f.base, at)
	if v != nil {
		rec.Verdict = *v
	}
	must(t, fx.store.Put(rec))
}

func newApprovalFixture(t *testing.T) *approvalFixture {
	t.Helper()
	f, l, _ := keyFixture(t)
	// The approval opens the worktree through l.open: the fake's origin stands in.
	l.openRepo = func(string) Repo { return f.git.Open(f.origin) }
	g := f.git.Open(f.origin)
	f.git.SetRef(t, f.origin, "refs/heads/candidate", f.base)
	merged := f.git.Commit(t, f.origin, "candidate", "land: merge onto main", map[string]string{"b.txt": "work\n"})
	store := TownAheadStore(f.town, "gastown")
	store.Now = l.Now
	l.Ahead, l.ReuseExpiry = store, time.Hour
	issue, err := l.Beads.Show(f.work.BeadID)
	must(t, err)
	in, _, _, err := l.reviewInputFor(issue, f.work, f.base, merged, f.origin, g, true)
	must(t, err)
	key, _, err := l.reviewKey(context.Background(), g, g, f.work, in, f.base, merged)
	must(t, err)
	fx := &approvalFixture{f: f, l: l, g: g, dir: f.origin, merged: merged, store: store, key: key}
	fx.put(t, l.now().Add(-time.Minute), nil)
	return fx
}

// TestGateAheadApproval: only a stored review approving the merge passes;
// every other answer is a named refusal with no bead written (gt-0hz0sy).
func TestGateAheadApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(t *testing.T, fx *approvalFixture)
		want string // substring of the refusal; "" means nil
	}{
		{name: "stored approval passes"},
		{name: "no stored review", edit: func(t *testing.T, fx *approvalFixture) { must(t, fx.store.Delete(fx.f.work.BeadID)) }, want: "no stored review is kept"},
		{name: "expired", edit: func(t *testing.T, fx *approvalFixture) { fx.put(t, fx.l.now().Add(-2*time.Hour), nil) }, want: "not reusable"},
		{name: "requests changes", edit: func(t *testing.T, fx *approvalFixture) {
			fx.put(t, fx.l.now().Add(-time.Minute), &Verdict{Verdict: VerdictRequestChanges, Score: 0.2})
		}, want: "does not approve"},
		{name: "findings block", edit: func(t *testing.T, fx *approvalFixture) {
			fx.put(t, fx.l.now().Add(-time.Minute), &Verdict{Verdict: VerdictApprove, Score: 0.9,
				Findings: []Finding{{ID: "f1", Severity: omSeverityMajor, Path: "b.txt"}}})
		}, want: "findings that block"},
		{name: "om config edit", edit: func(t *testing.T, fx *approvalFixture) {
			fx.f.git.SetRef(t, fx.f.origin, "refs/heads/om-edit", fx.f.base)
			fx.merged = fx.f.git.Commit(t, fx.f.origin, "om-edit", "land: raise the bar", map[string]string{omConfigFile: `{"threshold":0.95}`})
		}, want: omConfigFile},
		{name: "input assembly fails", edit: func(t *testing.T, fx *approvalFixture) { fx.l.Beads = aheadCommentsFails{fx.f.bd} }, want: "could not be assembled"},
		{name: "bead unreadable", edit: func(t *testing.T, fx *approvalFixture) { fx.f.bd.FailWith("show", errors.New("bd is down")) }, want: "reading bead"},
		{name: "no bead returned", edit: func(t *testing.T, fx *approvalFixture) {
			fx.l.Beads = gateAheadNilShowBeads{fx.l.Beads}
		}, want: "no bead returned"},
		{name: "store off", edit: func(t *testing.T, fx *approvalFixture) { fx.l.Ahead = nil }, want: "no review-ahead store"},
		{name: "record unreadable", edit: func(t *testing.T, fx *approvalFixture) {
			fx.store.readFile = func(string) ([]byte, error) { return []byte("{not a record"), nil }
		}, want: "could not be read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newApprovalFixture(t)
			if tc.edit != nil {
				tc.edit(t, fx)
			}
			before := aheadBeadWrites(t, fx.f.bd)
			err := fx.l.gateAheadApproval(fx.g, fx.f.work, fx.f.base, fx.dir, fx.merged)
			if n := aheadBeadWrites(t, fx.f.bd) - before; n != 0 {
				t.Errorf("gateAheadApproval wrote %d bead records, want 0", n)
			}
			if tc.want == "" {
				if err != nil {
					t.Fatalf("gateAheadApproval = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("gateAheadApproval = %v; want an error containing %q", err, tc.want)
			}
		})
	}
}
