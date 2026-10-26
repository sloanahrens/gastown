package scorecard

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/land"
)

const (
	// reviewDocsDir is where a polecat writes its review of a landed head.
	reviewDocsDir = "docs/reviews"
	// reviewTarget is the branch the documents are read from: the branch the
	// heads they review landed on.
	reviewTarget = "refs/heads/main"
)

// reviewDocDate is the date a review document's name opens with, as in
// docs/reviews/2026-10-26-gt-xzo8a.md.
var reviewDocDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)

// reviewDocs reads the polecat review verdicts recorded for a bead.
type reviewDocs interface {
	// Verdict returns the verdict the bead's review documents record for head,
	// and whether one records it.
	Verdict(ctx context.Context, bead, head string) (string, bool, error)
}

// cloneReviews reads review documents from the town clone's main branch. It
// lists the directory once and reads each document once, so labeling a long
// landing log costs one git call per document, not per landing.
type cloneReviews struct {
	git cloneGit

	mu      sync.Mutex
	listed  bool
	byBead  map[string][]string
	docs    map[string]string
	listErr error
}

// Verdict returns the verdict of the latest document for bead that names head.
// Documents are named by date, so path order is review order and a second look
// supersedes the first.
func (r *cloneReviews) Verdict(ctx context.Context, bead, head string) (string, bool, error) {
	paths, err := r.paths(ctx, bead)
	if err != nil {
		return "", false, err
	}
	verdict, found := "", false
	for _, path := range paths {
		text, err := r.text(ctx, path)
		if err != nil {
			return "", false, err
		}
		if v, ok := land.PolecatReviewVerdict(text, head); ok {
			verdict, found = v, true
		}
	}
	return verdict, found, nil
}

// paths returns the review documents for bead, listing the directory on the
// first call. A failed listing is kept, so every later call reports it instead
// of asking git again.
func (r *cloneReviews) paths(ctx context.Context, bead string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.listed {
		r.listed = true
		r.listErr = r.list(ctx)
	}
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.byBead[bead], nil
}

// list reads the review document paths from the clone. A repository without
// the directory has no reviews, which is not an error.
func (r *cloneReviews) list(ctx context.Context) error {
	r.byBead = map[string][]string{}
	r.docs = map[string]string{}
	out, err := r.git.Run(ctx, "ls-tree", "-r", "--name-only", reviewTarget, "--", reviewDocsDir)
	if err != nil {
		return fmt.Errorf("listing %s: %w", reviewDocsDir, err)
	}
	for _, path := range strings.Fields(out) {
		bead, ok := reviewDocBead(path)
		if !ok {
			continue
		}
		r.byBead[bead] = append(r.byBead[bead], path)
	}
	for _, paths := range r.byBead {
		slices.Sort(paths)
	}
	return nil
}

// text returns the document at path, reading it on the first call.
func (r *cloneReviews) text(ctx context.Context, path string) (string, error) {
	r.mu.Lock()
	text, ok := r.docs[path]
	r.mu.Unlock()
	if ok {
		return text, nil
	}
	text, err := r.git.Run(ctx, "show", reviewTarget+":"+path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	r.mu.Lock()
	r.docs[path] = text
	r.mu.Unlock()
	return text, nil
}

// reviewDocBead names the bead a review document reviews: the file name
// without its .md suffix and leading date, so
// docs/reviews/2026-10-26-gt-xzo8a.md reviews gt-xzo8a.
func reviewDocBead(path string) (string, bool) {
	name, ok := strings.CutSuffix(filepath.Base(path), ".md")
	if !ok {
		return "", false
	}
	name = reviewDocDate.ReplaceAllString(name, "")
	return name, name != ""
}
