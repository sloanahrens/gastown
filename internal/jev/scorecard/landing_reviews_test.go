package scorecard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// fakeCloneGit answers the two calls cloneReviews makes: the listing call and
// the read of one document. err fails every call; readErr fails one document.
type fakeCloneGit struct {
	tree    string
	docs    map[string]string
	err     error
	readErr map[string]error
}

func (g *fakeCloneGit) Run(_ context.Context, args ...string) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	switch args[0] {
	case "ls-tree":
		return g.tree, nil
	case "show":
		path, ok := strings.CutPrefix(args[1], reviewTarget+":")
		if !ok {
			return "", fmt.Errorf("unexpected show argument %q", args[1])
		}
		if err := g.readErr[path]; err != nil {
			return "", err
		}
		return g.docs[path], nil
	}
	return "", fmt.Errorf("unexpected git %v", args)
}

// TestCloneReviewsFailsWhenADocumentCannotBeRead reports a document's read
// failure, naming the document, instead of reading it as no verdict.
func TestCloneReviewsFailsWhenADocumentCannotBeRead(t *testing.T) {
	t.Parallel()
	head := "a1a1a1a"
	path := reviewDocsDir + "/2026-10-26-gt-x.md"
	git := &fakeCloneGit{
		tree:    path,
		docs:    map[string]string{path: land.PolecatReviewMarker + " " + head + " PASS"},
		readErr: map[string]error{path: errors.New("object not found")},
	}
	r := &cloneReviews{git: git}
	_, _, err := r.Verdict(context.Background(), "gt-x", head)
	if err == nil || !strings.Contains(err.Error(), "object not found") {
		t.Fatalf("Verdict error = %v, want the document read failure", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Verdict error = %v, want it to name %s", err, path)
	}
}

// TestCloneReviewsReadsTheLatestVerdict reads the verdict line naming the head
// from the bead's documents, a later document superseding an earlier one
// (gt-b9j3o.1).
func TestCloneReviewsReadsTheLatestVerdict(t *testing.T) {
	t.Parallel()
	head := "a1a1a1a"
	other := "b2b2b2b"
	first := reviewDocsDir + "/2026-10-25-gt-x.md"
	second := reviewDocsDir + "/2026-10-26-gt-x.md"
	git := &fakeCloneGit{
		tree: first + "\n" + second + "\n" + reviewDocsDir + "/notes.txt\n",
		docs: map[string]string{
			first:  "review\n" + land.PolecatReviewMarker + " " + head + " PASS\n",
			second: "second look\n" + land.PolecatReviewMarker + " " + head + " FAIL: internal/x.go:12 ignores the error\n",
		},
	}
	r := &cloneReviews{git: git}
	for _, tc := range []struct {
		name  string
		bead  string
		head  string
		want  string
		found bool
	}{
		{"the later document's FAIL stands", "gt-x", head, land.OverseerReviewFail, true},
		{"another head has no verdict", "gt-x", other, "", false},
		{"a bead without documents has no verdict", "gt-y", head, "", false},
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

// TestCloneReviewsKeepsAFailedListing fails every read after the first, so a
// clone whose listing broke never passes as no reviews.
func TestCloneReviewsKeepsAFailedListing(t *testing.T) {
	t.Parallel()
	git := &fakeCloneGit{err: errors.New("no clone")}
	r := &cloneReviews{git: git}
	for range 2 {
		_, _, err := r.Verdict(context.Background(), "gt-x", "a1a1a1a")
		if err == nil || !strings.Contains(err.Error(), "no clone") {
			t.Fatalf("Verdict error = %v, want the listing failure", err)
		}
	}
}

// TestReviewDocBead names the bead a document reviews from its file name.
func TestReviewDocBead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		bead string
		ok   bool
	}{
		{"docs/reviews/2026-10-26-gt-xzo8a.md", "gt-xzo8a", true},
		{"docs/reviews/2026-10-26-gt-p85pw.9.md", "gt-p85pw.9", true},
		{"docs/reviews/gt-abc.md", "gt-abc", true},
		{"docs/reviews/notes.txt", "", false},
		{"docs/reviews/2026-10-26-.md", "", false},
	} {
		bead, ok := reviewDocBead(tc.path)
		if bead != tc.bead || ok != tc.ok {
			t.Errorf("reviewDocBead(%q) = %q, %v; want %q, %v", tc.path, bead, ok, tc.bead, tc.ok)
		}
	}
}
