package estimate

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// fencedList is a description holding one fenced list of n one-line items.
func fencedList(n int) string {
	var b strings.Builder
	b.WriteString("## Goal\nPlace commands.\n\nCommand paths this bead places:\n```\n")
	for i := 0; i < n; i++ {
		b.WriteString("gt command " + strings.Repeat("x", i%5) + "\n")
	}
	b.WriteString("```\n\n## Size\nOne worker.\n")
	return b.String()
}

func boxes(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("- [ ] a box\n")
	}
	return b.String()
}

func TestSizeMinutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		desc   string
		accept string
		want   float64
		ok     bool
	}{
		{"five commands, three boxes", fencedList(5), boxes(3), 9.5, true},
		{"four boxes still counts", fencedList(5), boxes(4), 9.5, true},
		{"five boxes is more than a list says", fencedList(5), boxes(5), 0, false},
		{"two items is not an enumeration", fencedList(2), boxes(3), 0, false},
		{"no fenced list", "## Goal\nDo it.\n", boxes(3), 0, false},
		{"forty commands imply more than any cap", fencedList(40), boxes(3), 62, true},
		{"unclosed fence counts to the end", "x\n```\na\nb\nc\nd\n", boxes(2), 8, true},
		{"boxes from the description when the field is empty", fencedList(5) + "\n## Acceptance\n" + boxes(5), "", 0, false},
		{"largest of two lists", fencedList(3) + "\n```\na\nb\nc\nd\ne\nf\n```\n", boxes(2), 11, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := sizeMinutes(&beads.Issue{Description: tc.desc, AcceptanceCriteria: tc.accept})
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("sizeMinutes = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// blendAt is the blend seam answering a fixed number of minutes, so the cap is
// driven against a known blend.
func blendAt(minutes float64) func(storypoints.Inputs) (storypoints.Blended, error) {
	return func(storypoints.Inputs) (storypoints.Blended, error) {
		p, err := storypoints.FromMinutes(minutes)
		return storypoints.Blended{Minutes: minutes, Points: p, Inputs: []string{"shape"}, Rung: "shape"}, err
	}
}

// TestEstimateCapsTheBlendWithTheSizeInput: a bead that lists five commands is
// no larger than its list says (9.5 minutes, sp:8) though the blend reads 14.5;
// the note's inputs line names the size input; a bead listing forty commands is
// not lowered; a bead the cap would raise is left alone.
func TestEstimateCapsTheBlendWithTheSizeInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		items      int
		blend      float64
		wantPoints int
		wantMin    float64
		wantSize   bool
	}{
		{"five commands under a 14.5 blend", 5, 14.5, 8, 9.5, true},
		{"forty commands stay at the blend", 40, 14.5, 13, 14.5, false},
		{"a blend already under the list is not raised", 5, 6.0, 5, 6.0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := sampleBead()
			issue.Description = fencedList(tc.items)
			issue.AcceptanceCriteria = boxes(3)

			got, err := estimateWith(context.Background(), Deps{}, issue, blendAt(tc.blend))
			if err != nil {
				t.Fatalf("estimateWith: %v", err)
			}
			if got.Points != tc.wantPoints {
				t.Errorf("points = %d, want %d", got.Points, tc.wantPoints)
			}
			if got.Minutes != tc.wantMin {
				t.Errorf("minutes = %v, want %v", got.Minutes, tc.wantMin)
			}
			named := strings.Contains(Note(got), "inputs: shape, size")
			if named != tc.wantSize {
				t.Errorf("note names the size input = %v, want %v:\n%s", named, tc.wantSize, Note(got))
			}
		})
	}
}
