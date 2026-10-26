//go:build integration

package scorecard

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// TestIntegrationCloneReviewsReadsTheRepo drives the review-document reader
// over a real repository: the bead is named by a dated file, and only the
// verdict line for the reviewed head counts. Documents name the head by an
// abbreviation and the query gives it whole, as a landing record does
// (gt-b9j3o.1).
func TestIntegrationCloneReviewsReadsTheRepo(t *testing.T) {
	t.Parallel()
	head := "b25963ac729a403d8b5a7dcb90f03a8bf11a6ce3"
	other := "c46d60c1aaaabbbbccccddddeeeeffff00001111"
	dir := fixtureRepo(t, []fixtureCommit{{
		at: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
		files: map[string]string{
			"docs/reviews/2026-10-26-gt-x.md": "review of the landing\n" +
				land.PolecatReviewMarker + " " + head[:13] + " PASS\n",
			"docs/reviews/2026-10-24-gt-y.md": "second look\n" +
				land.PolecatReviewMarker + " " + other[:12] + " FAIL: internal/x.go:12 ignores the error\n",
			"docs/reviews/notes.txt": "not a review document",
		},
	}})
	r := &cloneReviews{git: repoGit{dir: dir}}
	for _, tc := range []struct {
		name  string
		bead  string
		head  string
		want  string
		found bool
	}{
		{"the dated file names the bead it reviews", "gt-x", head, land.OverseerReviewPass, true},
		{"a FAIL reads as a verdict", "gt-y", other, land.OverseerReviewFail, true},
		{"a head the document does not name has no verdict", "gt-x", other, "", false},
		{"a bead without a document has no verdict", "gt-z", head, "", false},
	} {
		got, found, err := r.Verdict(context.Background(), tc.bead, tc.head)
		if err != nil {
			t.Fatalf("%s: Verdict error = %v", tc.name, err)
		}
		if got != tc.want || found != tc.found {
			t.Errorf("%s: Verdict = %q, %v; want %q, %v", tc.name, got, found, tc.want, tc.found)
		}
	}
}
