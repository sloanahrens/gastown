package cmd

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// deriveFunc re-derives the pair the registry holds for a commit, the shape of
// deriveUpdateImagesPair with its seams bound.
type deriveFunc func(ctx context.Context, sha string) (imagevalues.Values, error)

// pairMatchesRegistry refuses a values file whose pair is not the one the
// registry derives for the file's commit, and refuses a file whose derivation
// fails, wrapping the cause. An install with no derivation wired refuses every
// file before any comparison: a missing derivation is never a match.
func pairMatchesRegistry(ctx context.Context, derive deriveFunc, file imagevalues.Values) error {
	if derive == nil {
		return fmt.Errorf("the install has no registry derivation wired, so the file for commit %s cannot be matched to a derivation", file.Commit)
	}
	derived, err := derive(ctx, file.Commit)
	if err != nil {
		return fmt.Errorf("the pair the registry derives for commit %s cannot be read, so the file cannot be matched to a derivation: %w", file.Commit, err)
	}
	return pairMatchesDerived(file, derived)
}
