package fixrecurrence

import (
	"slices"
	"testing"
)

const (
	// wantTitleConfidence is the confidence a title rule carries, stated here
	// rather than read from the implementation.
	wantTitleConfidence = 0.9
	// wantDescriptionConfidence is a description's weight against the title
	// rule it reuses: the title's confidence times 0.9.
	wantDescriptionConfidence = 0.81
)

func TestPropose(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		beads []Bead
		want  []Proposal
	}{
		{
			name: "the title's rule decides the proposal",
			beads: []Bead{
				{ID: "gt-0nk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-0nk: the check fails open"},
			},
			want: []Proposal{
				{New: "gt-me9t", Old: "gt-0nk", Rel: Recurrence, Confidence: wantTitleConfidence, Source: "title"},
			},
		},
		{
			name: "the title's rule wins over the description's",
			beads: []Bead{
				{ID: "gt-prk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title:       "gt-prk (bb9c373) broke the guard",
					Description: "same class as gt-prk: another flag"},
			},
			want: []Proposal{
				{New: "gt-me9t", Old: "gt-prk", Rel: Regression, Confidence: wantTitleConfidence, Source: "title"},
			},
		},
		{
			name: "a passing title leaves the description to classify the pair",
			beads: []Bead{
				{ID: "gt-0nk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title:       "see gt-0nk for the context",
					Description: "This is a recurrence of gt-0nk: the check fails open"},
			},
			want: []Proposal{
				{New: "gt-me9t", Old: "gt-0nk", Rel: Recurrence, Confidence: wantDescriptionConfidence, Source: "description"},
			},
		},
		{
			name: "a title citation no rule reads is ambiguous",
			beads: []Bead{
				{ID: "gt-0nk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title:       "see gt-0nk for the context",
					Description: "the parser drops the flag at the end of a line"},
			},
			want: []Proposal{
				{New: "gt-me9t", Old: "gt-0nk", Rel: Reference, Source: "ambiguous", Ambiguous: true},
			},
		},
		{
			name: "the ambiguous title citation matches the id case-insensitively",
			beads: []Bead{
				{ID: "gt-0nk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "See GT-0NK in the log"},
			},
			want: []Proposal{
				{New: "gt-me9t", Old: "gt-0nk", Rel: Reference, Source: "ambiguous", Ambiguous: true},
			},
		},
		{
			name: "a title naming three beads claims none of them",
			beads: []Bead{
				{ID: "gt-0nk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-prk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-0nk, gt-4mnd and gt-prk"},
			},
			want: []Proposal{
				{New: "gt-me9t", Old: "gt-0nk", Rel: Reference, Source: "ambiguous", Ambiguous: true},
				{New: "gt-me9t", Old: "gt-4mnd", Rel: Reference, Source: "ambiguous", Ambiguous: true},
				{New: "gt-me9t", Old: "gt-prk", Rel: Reference, Source: "ambiguous", Ambiguous: true},
			},
		},
		{
			name: "a pair neither classifier names and the title does not cite yields nothing",
			beads: []Bead{
				{ID: "gt-0nk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title:       "the parser drops the flag",
					Description: "written after reading gt-0nk for background"},
			},
		},
		{
			name: "an id the title uses inside a longer token is no citation",
			beads: []Bead{
				{ID: "gt-0nk", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title:       "the branch gt-0nk2 is stale",
					Description: "written after reading gt-0nk for background"},
			},
		},
		{
			name: "the proposals are sorted by new id then old id",
			beads: []Bead{
				{ID: "gt-new2", Status: "open", CreatedAt: at(10), Title: "follow-up to gt-bbb"},
				{ID: "gt-ccc", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-bbb", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-new", Status: "open", CreatedAt: at(10), Title: "recurrence of gt-ccc"},
				{ID: "gt-aaa", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-new3", Status: "open", CreatedAt: at(10), Title: "recurrence of gt-aaa"},
			},
			want: []Proposal{
				{New: "gt-new", Old: "gt-ccc", Rel: Recurrence, Confidence: wantTitleConfidence, Source: "title"},
				{New: "gt-new2", Old: "gt-bbb", Rel: FollowUp, Confidence: wantTitleConfidence, Source: "title"},
				{New: "gt-new3", Old: "gt-aaa", Rel: Recurrence, Confidence: wantTitleConfidence, Source: "title"},
			},
		},
		{
			name: "no beads make no proposal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Propose(tt.beads); !slices.Equal(got, tt.want) {
				t.Errorf("Propose() = %v, want %v", got, tt.want)
			}
		})
	}
}
