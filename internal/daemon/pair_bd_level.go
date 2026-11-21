package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
)

// pairBdLevelCommit is the shape of a commit PairBdLevel reads: a full lowercase
// sha, so a value that could pass for a git option never reaches git's argv.
var pairBdLevelCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PairBdLevel returns the bd commit the image pins of commit name and the schema
// level that bd commit carries, read as the pair check reads them: the pins file
// from the gastown rig's bare clone, the level from the beads rig's. An error
// names the read that failed, and never a level.
func PairBdLevel(ctx context.Context, townRoot, commit string) (pin string, level int, err error) {
	clone := git.NewGit(constants.RigBareRepoPath(filepath.Join(townRoot, "gastown")))
	return pairBdLevelAt(ctx, townRoot, commit, pairReadyPinsAt(clone), pairReadySkewGitRun)
}

// pairBdLevelAt is PairBdLevel over the two reads it makes: pins reads the pins
// file at a commit (nil is a rig with no clone) and run is the lister's git.
func pairBdLevelAt(ctx context.Context, townRoot, commit string, pins pairReadyPinsRead, run func(ctx context.Context, dir string, args ...string) ([]byte, error)) (string, int, error) {
	if !pairBdLevelCommit.MatchString(commit) {
		return "", 0, fmt.Errorf("commit %q is not a full sha", commit)
	}
	if pins == nil {
		return "", 0, errors.New("the rig has no local clone to read the bd pin from")
	}
	text, err := pins(commit)
	if err != nil {
		return "", 0, fmt.Errorf("reading the image pins at %s: %w", commit, err)
	}
	parsed, err := parseImagePins([]byte(text), func() (string, error) { return config.ResolveForgeOwner(townRoot) })
	if err != nil {
		return "", 0, fmt.Errorf("reading the bd pin from the image pins at %s: %w", commit, err)
	}
	pin := pairReadyBdPin(parsed)
	level, err := pairReadySkewReadsAt(townRoot, run, nil).LevelAt(ctx, pin)
	if err != nil {
		return pin, 0, fmt.Errorf("the schema level bd %s carries: %w", greenRefShortSHA(pin), err)
	}
	return pin, level, nil
}
