package daemon

import (
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// The rounds ledger is written only when om reviews the landing: with review
// off there is no om to hand the ledger to (gt-yphcl).
func TestRoundsDirForIsSetOnlyWhenOMReviews(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		reviewer land.Reviewer
		want     string
	}{
		{name: "om review on", reviewer: land.OMReviewer{}, want: "/town/om"},
		{name: "review off", reviewer: land.SkipReviewer{}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := roundsDirFor(tc.reviewer, "/town/om"); got != tc.want {
				t.Errorf("roundsDirFor = %q, want %q", got, tc.want)
			}
		})
	}
}
