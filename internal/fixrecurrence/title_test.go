package fixrecurrence

import (
	"strings"
	"testing"
)

// padding divides a title from the id it cites without itself matching a rule.
const padding = "padding "

func TestClassifyTitle(t *testing.T) {
	tests := []struct {
		name           string
		title          string
		id             string
		want           Relationship
		wantConfidence float64
	}{
		{
			name:           "recurrence of the cited bead",
			title:          "recurrence of gt-0nk: the check fails open",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "regression names the cited bead and its commit",
			title:          "gt-prk (bb9c373) broke the guard",
			id:             "gt-prk",
			want:           Regression,
			wantConfidence: 0.9,
		},
		{
			name:           "follow-up to the cited bead",
			title:          "Follow-up to gt-me9t: port the fix",
			id:             "gt-me9t",
			want:           FollowUp,
			wantConfidence: 0.9,
		},
		{
			name:           "same bug class as the cited bead",
			title:          "same bug class as gt-4mnd: another flag",
			id:             "gt-4mnd",
			want:           SameClass,
			wantConfidence: 0.9,
		},
		{
			name:           "three ids claim no single relationship",
			title:          "recurrence of gt-0nk, gt-4mnd and gt-prk",
			id:             "gt-0nk",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "three ids keep the claim when the grouping is named",
			title:          "recurrence of gt-0nk, gt-4mnd and gt-prk, same class of failure",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "a title that only cites the bead",
			title:          "see gt-0nk for background",
			id:             "gt-0nk",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "back after the cited bead",
			title:          "back after gt-4mnd: the cursor jumps",
			id:             "gt-4mnd",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "a resurgence of the cited bead",
			title:          "resurfacing of gt-0nk: the guard fails open",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "the cited bead persists",
			title:          "gt-0nk persists: the check fails open",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "the cited bead is not fixed",
			title:          "gt-0nk is not fixed",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "the cited bead will reopen",
			title:          "gt-0nk will reopen",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "neither names the cited bead",
			title:          "neither gt-0nk nor gt-prk fails open",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "three ids with neither still claim the relationship",
			title:          "neither gt-0nk nor gt-4mnd nor gt-prk is fixed",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "the cited bead's fix is incomplete",
			title:          "gt-0nk's fix is incomplete: the check fails open again",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "the cited bead's fix only partly works",
			title:          "gt-0nk's fix only partly works",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "the weak word again follows the cited bead",
			title:          "gt-0nk fails open again",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.7,
		},
		{
			name:           "the weak word still follows the cited bead",
			title:          "gt-0nk fails open still",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.7,
		},
		{
			name:           "a weak word beyond the window is no claim",
			title:          "gt-0nk " + strings.Repeat(padding, 12) + "again",
			id:             "gt-0nk",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "an id ending a sentence is still a citation",
			title:          "recurrence of gt-0nk. the check fails open",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "unmasked by the cited bead",
			title:          "unmasked by gt-4mnd: the check fails open",
			id:             "gt-4mnd",
			want:           Regression,
			wantConfidence: 0.9,
		},
		{
			name:           "caused by the cited bead",
			title:          "caused by gt-prk: the guard panics",
			id:             "gt-prk",
			want:           Regression,
			wantConfidence: 0.9,
		},
		{
			name:           "introduced by the cited bead",
			title:          "introduced by gt-0nk: the flag is ignored",
			id:             "gt-0nk",
			want:           Regression,
			wantConfidence: 0.9,
		},
		{
			name:           "the cited bead broke the guard",
			title:          "gt-4mnd broke the guard",
			id:             "gt-4mnd",
			want:           Regression,
			wantConfidence: 0.9,
		},
		{
			name:           "an exact phrase beats the weak word",
			title:          "gt-prk broke the guard again",
			id:             "gt-prk",
			want:           Regression,
			wantConfidence: 0.9,
		},
		{
			name:           "same class as the earlier one follows the cited bead",
			title:          "gt-4mnd has the Same bug class as the earlier one",
			id:             "gt-4mnd",
			want:           SameClass,
			wantConfidence: 0.9,
		},
		{
			name:           "a same-class phrase beyond the window is no claim",
			title:          "gt-4mnd " + strings.Repeat(padding, 6) + "same class as the earlier one",
			id:             "gt-4mnd",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "follow-up follows the cited bead",
			title:          "gt-me9t follow-up: port the fix",
			id:             "gt-me9t",
			want:           FollowUp,
			wantConfidence: 0.9,
		},
		{
			name:           "residual follows the cited bead",
			title:          "gt-me9t residual: the check still skips the flag",
			id:             "gt-me9t",
			want:           FollowUp,
			wantConfidence: 0.9,
		},
		{
			name:           "leftover follows the cited bead",
			title:          "gt-me9t leftover: another flag name",
			id:             "gt-me9t",
			want:           FollowUp,
			wantConfidence: 0.9,
		},
		{
			name:           "the id in capitals is the same bead",
			title:          "Recurrence of GT-0NK: the check fails open",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "a second citation carries the claim",
			title:          "gt-0nk broke the guard; the recurrence of gt-0nk is back",
			id:             "gt-0nk",
			want:           Recurrence,
			wantConfidence: 0.9,
		},
		{
			name:           "an empty title",
			title:          "",
			id:             "gt-0nk",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "an empty id",
			title:          "recurrence of gt-0nk",
			id:             "",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "a title naming no id",
			title:          "the guard fails open",
			id:             "gt-0nk",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "an id that is a prefix of a longer id",
			title:          "gt-0nk2 broke the guard",
			id:             "gt-0nk",
			want:           Reference,
			wantConfidence: 0,
		},
		{
			name:           "an id that is the parent of a child id",
			title:          "gt-0nk.3 broke the guard",
			id:             "gt-0nk",
			want:           Reference,
			wantConfidence: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, confidence := ClassifyTitle(tt.title, tt.id)
			if got != tt.want {
				t.Fatalf("ClassifyTitle(%q, %q) = %q, want %q", tt.title, tt.id, got, tt.want)
			}
			if confidence != tt.wantConfidence {
				t.Errorf("ClassifyTitle(%q, %q) confidence = %v, want %v", tt.title, tt.id, confidence, tt.wantConfidence)
			}
		})
	}
}
