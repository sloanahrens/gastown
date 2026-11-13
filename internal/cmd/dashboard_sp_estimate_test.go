package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/polecat"
)

// A seat's row carries the estimate its bead's sp: label holds, on its own
// field, since the badge labels drop it. A bead with no estimate leaves the
// field absent from the row and the JSON (gt-cdhagm).
func TestDashboardSeatCarriesTheSPEstimate(t *testing.T) {
	t.Parallel()
	cap1 := polecat.WorkstateDisposition{CountsTowardCapacity: true}
	pcs := buildDashPolecats(dashPolecatInputs{Now: time.Now(), SessionsKnown: true, Seats: []dashSeat{
		{Rig: "gastown", Name: "sized", Item: dashItem(polecat.StateWorking, cap1), Issue: &beads.Issue{ID: "gt-1", Title: "one", Labels: []string{"sp:8"}}},
		{Rig: "gastown", Name: "unsized", Item: dashItem(polecat.StateWorking, cap1), Issue: &beads.Issue{ID: "gt-2", Title: "two"}},
	}})
	got := map[string]*int{}
	for _, p := range pcs {
		got[p.Name] = p.SPEstimate
	}
	if got["sized"] == nil || *got["sized"] != 8 {
		t.Errorf("seat with sp:8 has estimate %v, want 8", got["sized"])
	}
	if got["unsized"] != nil {
		t.Errorf("seat with no sp: label has estimate %d, want absent", *got["unsized"])
	}
	bare, err := json.Marshal(dashboard.Polecat{Name: "unsized"})
	if err != nil || strings.Contains(string(bare), "sp_estimate") {
		t.Errorf("a seat with no estimate must omit sp_estimate: %s err=%v", bare, err)
	}
}

// A queued row carries the estimate its bead's sp: label holds, and a bead with
// no estimate leaves it absent (gt-cdhagm).
func TestDashboardQueueRowCarriesTheSPEstimate(t *testing.T) {
	t.Parallel()
	st := &fakeQueueStore{landing: []*beads.Issue{
		{ID: "gt-1", Title: "sized", Status: "open", Labels: []string{land.LabelReadyToLand, "sp:3"}},
		{ID: "gt-2", Title: "unsized", Status: "open", Labels: []string{land.LabelReadyToLand}},
	}}
	q := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown").read(time.Now())
	if q == nil || len(q.Landing) != 2 {
		t.Fatalf("landing = %+v; want both beads queued", q)
	}
	got := map[string]*int{}
	for _, row := range q.Landing {
		got[row.ID] = row.SPEstimate
	}
	if got["gt-1"] == nil || *got["gt-1"] != 3 {
		t.Errorf("queued bead with sp:3 has estimate %v, want 3", got["gt-1"])
	}
	if got["gt-2"] != nil {
		t.Errorf("queued bead with no sp: label has estimate %d, want absent", *got["gt-2"])
	}
}

// A landed row carries the estimate its record holds, and its actual is the
// points its polecat time earns. The actual is absent when the time is a lower
// bound, an upper bound, or absent, and it needs no sp: label (gt-cdhagm).
func TestDashboardLandedRowSPActual(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
	five := 5
	tests := []struct {
		name       string
		rec        landings.Record
		wantEst    *int
		wantActual *int
	}{
		{"exact time with an estimate", landings.Record{PolecatSecs: 600, SPEstimate: &five}, &five, ptrInt(8)},
		{"exact time with no estimate", landings.Record{PolecatSecs: 600}, nil, ptrInt(8)},
		{"lower-bound time", landings.Record{PolecatSecs: 600, PolecatLowerBound: true, SPEstimate: &five}, &five, nil},
		{"upper-bound time", landings.Record{PolecatSecs: 600, PolecatUpperBound: true, SPEstimate: &five}, &five, nil},
		{"absent time", landings.Record{SPEstimate: &five}, &five, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.rec.Bead, tt.rec.Rig, tt.rec.OMVerdict = "gt-abc", "gastown", "approve"
			tt.rec.LandedAt = now.Add(-time.Minute)
			rows := buildRecentLandings(now, []omRecord{{Record: tt.rec}}, nil, nil, nil, nil, nil, landingNames{}, nil, nil, nil, nil, recentLandingRows)
			if len(rows) != 1 {
				t.Fatalf("rows = %d; want the one landing", len(rows))
			}
			row := rows[0]
			if !sameIntPtr(row.SPEstimate, tt.wantEst) {
				t.Errorf("sp estimate = %v, want %v", row.SPEstimate, tt.wantEst)
			}
			if !sameIntPtr(row.SPActual, tt.wantActual) {
				t.Errorf("sp actual = %v, want %v", row.SPActual, tt.wantActual)
			}
			if tt.wantActual == nil {
				b, err := json.Marshal(row)
				if err != nil || strings.Contains(string(b), "sp_actual") {
					t.Errorf("a row with no actual must omit sp_actual: %s err=%v", b, err)
				}
			}
		})
	}
}

// ptrInt is a pointer to n, for table rows.
func ptrInt(n int) *int { return &n }

// sameIntPtr reports whether two optional ints are both absent or both equal.
func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
