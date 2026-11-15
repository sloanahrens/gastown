package fixrecurrence

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

var (
	// reportSince and reportUntil are the window every row uses unless the row
	// is about a boundary.
	reportSince = time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	reportUntil = time.Date(2020, 3, 15, 0, 0, 0, 0, time.UTC)
	// windowMid is a moment inside the window, so a row that is not about the
	// boundary lists its closed bugs without repeating a timestamp.
	windowMid = reportSince.Add(7 * 24 * time.Hour)
)

// closedBeads is the same close time for each id, which is all a row about
// anything but the window boundary needs.
func closedBeads(at time.Time, ids ...string) []Closed {
	closed := make([]Closed, len(ids))
	for i, id := range ids {
		closed[i] = Closed{ID: id, ClosedAt: at}
	}
	return closed
}

func TestReportHeldRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		closed []Closed
		edges  []Edge
		want   ReportResult
	}{
		{
			name: "ten closed bugs with two failing give a rate of eight tenths",
			closed: closedBeads(windowMid,
				"gt-ol01", "gt-ol02", "gt-ol03", "gt-ol04", "gt-ol05",
				"gt-ol06", "gt-ol07", "gt-ol08", "gt-ol09", "gt-ol10"),
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol02", Rel: Regression},
			},
			want: ReportResult{Held: 8, Total: 10, Rate: 0.8},
		},
		{
			name:   "a follow-up alone leaves the fix held",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02", "gt-ol03"),
			edges:  []Edge{{New: "gt-nw01", Old: "gt-ol02", Rel: FollowUp}},
			want:   ReportResult{Held: 3, Total: 3, Rate: 1},
		},
		{
			name:   "a reference alone leaves the fix held",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02"),
			edges:  []Edge{{New: "gt-nw01", Old: "gt-ol01", Rel: Reference}},
			want:   ReportResult{Held: 2, Total: 2, Rate: 1},
		},
		{
			name:   "a same-class edge fails the fix",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02", "gt-ol03", "gt-ol04"),
			edges:  []Edge{{New: "gt-nw01", Old: "gt-ol03", Rel: SameClass}},
			want:   ReportResult{Held: 3, Total: 4, Rate: 0.75},
		},
		{
			name:   "an edge to a bug closed outside the window is not a failure",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02"),
			edges:  []Edge{{New: "gt-nw01", Old: "gt-ol09", Rel: Recurrence}},
			want:   ReportResult{Held: 2, Total: 2, Rate: 1},
		},
		{
			name:   "a self edge is not a failure",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02"),
			edges:  []Edge{{New: "gt-ol01", Old: "gt-ol01", Rel: Recurrence}},
			want:   ReportResult{Held: 2, Total: 2, Rate: 1},
		},
		{
			name:   "a self edge spelled in another case is not a failure",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02"),
			edges:  []Edge{{New: "GT-OL01", Old: "gt-ol01", Rel: Recurrence}},
			want:   ReportResult{Held: 2, Total: 2, Rate: 1},
		},
		{
			name:   "an edge finds a closed bug whose id is spelled in another case",
			closed: closedBeads(windowMid, "GT-OL01", "gt-ol02"),
			edges:  []Edge{{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence}},
			want:   ReportResult{Held: 1, Total: 2, Rate: 0.5},
		},
		{
			name:   "an edge spelled in another case still fails the fix",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02"),
			edges:  []Edge{{New: "gt-nw01", Old: "GT-OL01", Rel: Recurrence}},
			want:   ReportResult{Held: 1, Total: 2, Rate: 0.5},
		},
		{
			name:   "an edge with no citing bead is not a failure",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02"),
			edges:  []Edge{{Old: "gt-ol01", Rel: Recurrence}},
			want:   ReportResult{Held: 2, Total: 2, Rate: 1},
		},
		{
			name:   "a relationship the package does not define fails the fix",
			closed: closedBeads(windowMid, "gt-ol01", "gt-ol02"),
			edges:  []Edge{{New: "gt-nw01", Old: "gt-ol01", Rel: Relationship("unrecognised")}},
			want:   ReportResult{Held: 1, Total: 2, Rate: 0.5},
		},
		{
			name: "no closed bugs gives a rate of zero without dividing",
			want: ReportResult{Held: 0, Total: 0, Rate: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Report(tt.closed, tt.edges, reportSince, reportUntil)
			if got.Held != tt.want.Held || got.Total != tt.want.Total || got.Rate != tt.want.Rate {
				t.Errorf("Report() Held/Total/Rate = %d/%d/%v, want %d/%d/%v",
					got.Held, got.Total, got.Rate, tt.want.Held, tt.want.Total, tt.want.Rate)
			}
		})
	}
}

func TestReportChronic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		closed []Closed
		edges  []Edge
		want   []string
	}{
		{
			name:   "two strikes list a bug and one strike does not, sorted",
			closed: closedBeads(windowMid, "gt-ol03", "gt-ol01", "gt-ol02", "gt-ol04"),
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol03", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol03", Rel: SameClass},
				{New: "gt-nw03", Old: "gt-ol01", Rel: Regression},
				{New: "gt-nw04", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw05", Old: "gt-ol02", Rel: Recurrence},
			},
			want: []string{"gt-ol01", "gt-ol03"},
		},
		{
			name:   "one citing bead with two failing relationships is one strike",
			closed: closedBeads(windowMid, "gt-ol01"),
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw01", Old: "gt-ol01", Rel: SameClass},
			},
			want: nil,
		},
		{
			name:   "duplicate edges are one strike",
			closed: closedBeads(windowMid, "gt-ol01"),
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
			},
			want: nil,
		},
		{
			name:   "one citing bead spelled in two cases is one strike",
			closed: closedBeads(windowMid, "gt-ol01"),
			edges: []Edge{
				{New: "gt-NW01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw01", Old: "GT-OL01", Rel: Recurrence},
			},
			want: nil,
		},
		{
			name:   "strikes in another case list the closed record's spelling",
			closed: closedBeads(windowMid, "GT-OL01"),
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol01", Rel: Recurrence},
			},
			want: []string{"GT-OL01"},
		},
		{
			name: "a bug closed twice in two cases lists the earlier record's spelling",
			closed: []Closed{
				{ID: "GT-OL01", ClosedAt: windowMid},
				{ID: "gt-ol01", ClosedAt: windowMid.Add(time.Hour)},
			},
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol01", Rel: Recurrence},
			},
			want: []string{"GT-OL01"},
		},
		{
			name: "the close time, not the slice order, picks the spelling",
			closed: []Closed{
				{ID: "gt-ol01", ClosedAt: windowMid.Add(time.Hour)},
				{ID: "GT-OL01", ClosedAt: windowMid},
			},
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol01", Rel: Recurrence},
			},
			want: []string{"GT-OL01"},
		},
		{
			name: "two records closed at the same instant list the smaller spelling",
			closed: []Closed{
				{ID: "gt-ol01", ClosedAt: windowMid},
				{ID: "GT-OL01", ClosedAt: windowMid},
			},
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol01", Rel: Recurrence},
			},
			want: []string{"GT-OL01"},
		},
		{
			name:   "follow-ups alone never make a fix chronic",
			closed: closedBeads(windowMid, "gt-ol01"),
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: FollowUp},
				{New: "gt-nw02", Old: "gt-ol01", Rel: FollowUp},
			},
			want: nil,
		},
		{
			name:   "a follow-up is no second strike beside one failure",
			closed: closedBeads(windowMid, "gt-ol01"),
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol01", Rel: FollowUp},
			},
			want: nil,
		},
		{
			name:   "a bug closed outside the window is never chronic",
			closed: []Closed{{ID: "gt-ol01", ClosedAt: reportSince.Add(-time.Hour)}},
			edges: []Edge{
				{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
				{New: "gt-nw02", Old: "gt-ol01", Rel: Regression},
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Report(tt.closed, tt.edges, reportSince, reportUntil)
			if !slices.Equal(got.Chronic, tt.want) {
				t.Errorf("Report() Chronic = %v, want %v", got.Chronic, tt.want)
			}
		})
	}
}

func TestReportPerRelationship(t *testing.T) {
	t.Parallel()
	closed := closedBeads(windowMid, "gt-ol01", "gt-ol02")
	edges := []Edge{
		{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
		{New: "gt-nw01", Old: "gt-ol01", Rel: Recurrence},
		{New: "gt-nw02", Old: "gt-ol01", Rel: FollowUp},
		{New: "gt-nw03", Old: "gt-ol02", Rel: Reference},
		{New: "gt-nw04", Old: "gt-ol02", Rel: Relationship("unrecognised")},
		{New: "gt-nw05", Old: "gt-ol09", Rel: SameClass},
		{New: "gt-nw06", Old: "gt-ol02", Rel: ""},
	}
	want := map[Relationship]int{
		Recurrence:                   1,
		FollowUp:                     1,
		Reference:                    1,
		Relationship("unrecognised"): 1,
		Relationship(""):             1,
	}
	got := Report(closed, edges, reportSince, reportUntil)
	if !reflect.DeepEqual(got.PerRelationship, want) {
		t.Errorf("Report() PerRelationship = %v, want %v", got.PerRelationship, want)
	}
}

func TestReportPerRelationshipMatchesIDsCaseInsensitively(t *testing.T) {
	t.Parallel()
	closed := closedBeads(windowMid, "gt-ol01")
	edges := []Edge{
		{New: "gt-NW01", Old: "gt-ol01", Rel: Recurrence},
		{New: "gt-nw01", Old: "GT-OL01", Rel: Recurrence},
	}
	want := map[Relationship]int{Recurrence: 1}
	got := Report(closed, edges, reportSince, reportUntil)
	if !reflect.DeepEqual(got.PerRelationship, want) {
		t.Errorf("Report() PerRelationship = %v, want %v", got.PerRelationship, want)
	}
}

func TestReportWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		closed []Closed
		since  time.Time
		until  time.Time
		want   int
	}{
		{
			name:   "a close at the window start counts",
			closed: []Closed{{ID: "gt-ol01", ClosedAt: reportSince}},
			since:  reportSince,
			until:  reportUntil,
			want:   1,
		},
		{
			name:   "a close just inside the end counts",
			closed: []Closed{{ID: "gt-ol01", ClosedAt: reportUntil.Add(-time.Nanosecond)}},
			since:  reportSince,
			until:  reportUntil,
			want:   1,
		},
		{
			name:   "a close at the window end does not count",
			closed: []Closed{{ID: "gt-ol01", ClosedAt: reportUntil}},
			since:  reportSince,
			until:  reportUntil,
			want:   0,
		},
		{
			name:   "a close before the window does not count",
			closed: []Closed{{ID: "gt-ol01", ClosedAt: reportSince.Add(-time.Nanosecond)}},
			since:  reportSince,
			until:  reportUntil,
			want:   0,
		},
		{
			name:   "a record with no close time does not count",
			closed: []Closed{{ID: "gt-ol01"}},
			since:  reportSince,
			until:  reportUntil,
			want:   0,
		},
		{
			name:   "a record with no close time does not count in an open-ended window",
			closed: []Closed{{ID: "gt-ol01"}},
			since:  time.Time{},
			until:  reportUntil,
			want:   0,
		},
		{
			name:   "a record with no id does not count",
			closed: []Closed{{ClosedAt: windowMid}},
			since:  reportSince,
			until:  reportUntil,
			want:   0,
		},
		{
			name: "the same bug closed twice counts once",
			closed: []Closed{
				{ID: "gt-ol01", ClosedAt: windowMid},
				{ID: "gt-ol01", ClosedAt: windowMid.Add(time.Hour)},
			},
			since: reportSince,
			until: reportUntil,
			want:  1,
		},
		{
			name: "the same bug closed twice in two cases counts once",
			closed: []Closed{
				{ID: "GT-OL01", ClosedAt: windowMid},
				{ID: "gt-ol01", ClosedAt: windowMid.Add(time.Hour)},
			},
			since: reportSince,
			until: reportUntil,
			want:  1,
		},
		{
			name:   "an inverted window is empty",
			closed: []Closed{{ID: "gt-ol01", ClosedAt: windowMid}},
			since:  reportUntil,
			until:  reportSince,
			want:   0,
		},
		{
			name:   "a zero-length window is empty",
			closed: []Closed{{ID: "gt-ol01", ClosedAt: windowMid}},
			since:  windowMid,
			until:  windowMid,
			want:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Report(tt.closed, nil, tt.since, tt.until)
			if got.Total != tt.want {
				t.Errorf("Report() Total = %d, want %d", got.Total, tt.want)
			}
			wantRate := 0.0
			if tt.want > 0 {
				wantRate = 1
			}
			if got.Rate != wantRate {
				t.Errorf("Report() Rate = %v, want %v", got.Rate, wantRate)
			}
		})
	}
}
