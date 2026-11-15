package fixrecurrence

import (
	"slices"
	"testing"
	"time"
)

// at is a fixed instant for these rows, so the tests read the beads' own
// timestamps and never the wall clock. The dates sit years in the past: a
// comparison against time.Now would pair every row whose cited bead closed at
// all, and the rows asserting no pair would fail.
func at(day int) time.Time {
	return time.Date(2020, time.January, day, 12, 0, 0, 0, time.UTC)
}

func TestPairs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		beads []Bead
		want  []Pair
	}{
		{
			name: "a bead closed before the new bead was created is a pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "follow-up to gt-4mnd: port the fix"},
			},
			want: []Pair{{New: "gt-me9t", Old: "gt-4mnd"}},
		},
		{
			name: "a bead closed after the new bead was created is no pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(9)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "follow-up to gt-4mnd: port the fix"},
			},
		},
		{
			name: "a bead closed at the instant of the creation is no pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(5)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "a closed bead with an unknown close time is no pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "a cited bead that is not closed is no pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "open", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "an edge from the new bead to the old skips the pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{
					ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title: "recurrence of gt-4mnd",
					Edges: map[string]bool{"gt-4mnd": true},
				},
			},
		},
		{
			name: "an edge from the old bead to the new skips the pair",
			beads: []Bead{
				{
					ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2),
					Edges: map[string]bool{"gt-me9t": true},
				},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "a bead citing itself is no pair",
			// The close precedes the creation, so only the self-citation
			// check refuses this row.
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(5), ClosedAt: at(1), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "a citation of an id the list does not hold is no pair",
			beads: []Bead{
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-zz9"},
			},
		},
		{
			name: "a bead with no id contributes no pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "an id the list holds twice makes no pair as the cited bead",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-4mnd", Status: "open", CreatedAt: at(3)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "an id the list holds twice makes no pair as the citing bead",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(6), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "an id in the description is found as well as one in the title",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{
					ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title:       "the guard fails open again",
					Description: "This is a recurrence of gt-4mnd: the check fails open",
				},
			},
			want: []Pair{{New: "gt-me9t", Old: "gt-4mnd"}},
		},
		{
			name: "the title and description citing the same id make one pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{
					ID: "gt-me9t", Status: "open", CreatedAt: at(5),
					Title:       "recurrence of gt-4mnd",
					Description: "gt-4mnd's fix did not hold under load",
				},
			},
			want: []Pair{{New: "gt-me9t", Old: "gt-4mnd"}},
		},
		{
			name: "an id that ends a sentence is still a citation",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "Follow-up to gt-4mnd."},
			},
			want: []Pair{{New: "gt-me9t", Old: "gt-4mnd"}},
		},
		{
			name: "a citation written in capitals names the bead",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "RECURRENCE OF GT-4MND"},
			},
			want: []Pair{{New: "gt-me9t", Old: "gt-4mnd"}},
		},
		{
			name: "a citation names a bead whose id is not lowercase",
			beads: []Bead{
				{ID: "GT-4MND", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
			want: []Pair{{New: "gt-me9t", Old: "GT-4MND"}},
		},
		{
			name: "a bead citing its own id in another case is no pair",
			// The close precedes the creation, so only the self-citation
			// check refuses this row.
			beads: []Bead{
				{ID: "GT-ME9T", Status: "closed", CreatedAt: at(5), ClosedAt: at(1), Title: "recurrence of gt-me9t"},
			},
		},
		{
			name: "two ids that differ only in case name no single record",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "GT-4MND", Status: "open", CreatedAt: at(3)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "a citing id the list holds in two cases makes no pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "recurrence of gt-4mnd"},
				{ID: "GT-ME9T", Status: "open", CreatedAt: at(6), Title: "recurrence of gt-4mnd"},
			},
		},
		{
			name: "a citation glued to a longer token is no pair",
			beads: []Bead{
				{ID: "gt-wisp", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "see gt-wisp-l8d8bv for the record"},
			},
		},
		{
			name: "a citation glued to a longer token on the left is no pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "open", CreatedAt: at(5), Title: "the branch polecat-gt-4mnd is stale"},
			},
		},
		{
			name: "a citing bead that has since closed still makes a pair",
			beads: []Bead{
				{ID: "gt-4mnd", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-me9t", Status: "closed", CreatedAt: at(5), ClosedAt: at(6), Title: "recurrence of gt-4mnd"},
			},
			want: []Pair{{New: "gt-me9t", Old: "gt-4mnd"}},
		},
		{
			name: "the output is sorted by new id then old id",
			// The beads are listed so the expected order is neither the input
			// order nor the order the cited ids appear in a title.
			beads: []Bead{
				{ID: "gt-new2", Status: "open", CreatedAt: at(10), Title: "follow-up to gt-bbb"},
				{ID: "gt-ccc", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-bbb", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
				{ID: "gt-new", Status: "open", CreatedAt: at(10), Title: "same class as gt-ccc and recurrence of gt-aaa"},
				{ID: "gt-aaa", Status: "closed", CreatedAt: at(1), ClosedAt: at(2)},
			},
			want: []Pair{
				{New: "gt-new", Old: "gt-aaa"},
				{New: "gt-new", Old: "gt-ccc"},
				{New: "gt-new2", Old: "gt-bbb"},
			},
		},
		{
			name: "no beads make no pair",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Pairs(tt.beads); !slices.Equal(got, tt.want) {
				t.Errorf("Pairs(%v) = %v, want %v", tt.beads, got, tt.want)
			}
		})
	}
}
