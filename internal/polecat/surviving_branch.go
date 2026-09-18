package polecat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// rigGitRepo returns the git directory holding a rig's shared refs, mirroring
// Manager.repoBase: the shared bare repo (<rigRoot>/.repo.git) when present,
// otherwise the legacy <rigRoot>/mayor/rig clone. Returns "" when neither
// exists, which callers must treat as "cannot determine" (fail open).
func rigGitRepo(rigRoot string) string {
	bare := filepath.Join(rigRoot, ".repo.git")
	if info, err := os.Stat(bare); err == nil && info.IsDir() {
		return bare
	}
	legacy := filepath.Join(rigRoot, "mayor", "rig")
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy
	}
	return ""
}

// ListOriginPolecatBranches returns the polecat branch names present on the
// rig's "origin" remote, without the "refs/heads/" prefix. Callers that check
// many issues in one rig should list once and reuse MatchSurvivingBranches:
// each call is an ls-remote, and an unreachable remote costs the full query
// timeout.
func ListOriginPolecatBranches(rigRoot string) ([]string, error) {
	root := rigGitRepo(rigRoot)
	if root == "" {
		return nil, fmt.Errorf("no git repo under %s (neither .repo.git nor mayor/rig)", rigRoot)
	}

	var g *git.Git
	if filepath.Base(root) == ".repo.git" {
		g = git.NewGitWithDir(root, "")
	} else {
		g = git.NewGit(root)
	}

	refs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/polecat/")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, strings.TrimPrefix(r.Name, "refs/heads/"))
	}
	return names, nil
}

// FindSurvivingBranchesForIssue returns the generated polecat branches still
// present on the rig's origin remote that encode issueID, most recently
// generated first. Returns an empty slice (not an error) when the rig has no
// matching branch.
//
// This is the "is this bead's work already preserved somewhere?" predicate.
// A polecat killed mid-work — by a town halt, an operator park, or a crashed
// session — never runs `gt done`, so its branch stays on origin while the
// source bead still reads as stranded. Feeding that bead to a fresh polecat
// spawns a second worker from main on work that already exists (gt-ibt8 saw
// four polecats on one bead, gt-da2x three).
func FindSurvivingBranchesForIssue(rigRoot, issueID string) ([]string, error) {
	if strings.TrimSpace(issueID) == "" {
		return nil, nil
	}

	branches, err := ListOriginPolecatBranches(rigRoot)
	if err != nil {
		return nil, err
	}
	return MatchSurvivingBranches(branches, issueID), nil
}

// MatchSurvivingBranches filters branch names down to the generated polecat
// branches encoding issueID, most recently generated first.
func MatchSurvivingBranches(branches []string, issueID string) []string {
	if strings.TrimSpace(issueID) == "" {
		return nil
	}

	matches := make([]string, 0, 1)
	for _, branch := range branches {
		meta, ok := ParseGeneratedBranchName(branch)
		if !ok || meta.Issue != issueID {
			continue
		}
		matches = append(matches, branch)
	}
	if len(matches) < 2 {
		return matches
	}

	// Newest first. The generated suffix is
	// strconv.FormatInt(time.Now().UnixMilli(), 36) (Manager.buildBranchName),
	// so a larger base36 revision is a later branch. Suffixes that do not
	// parse as base36 — the "backup-<sha>" form written by preserve-then-nuke —
	// carry no recency and sort after the generated ones; the branch name
	// breaks remaining ties so the order is deterministic.
	sort.SliceStable(matches, func(i, j int) bool {
		ri, rj := branchRevision(matches[i], issueID), branchRevision(matches[j], issueID)
		ni, erri := strconv.ParseInt(ri, 36, 64)
		nj, errj := strconv.ParseInt(rj, 36, 64)
		if (erri == nil) != (errj == nil) {
			return erri == nil // generated revisions sort ahead of the rest
		}
		if erri == nil && ni != nj {
			return ni > nj
		}
		return matches[i] < matches[j]
	})
	return matches
}

// branchRevision returns the generated suffix following issueID in a branch
// name (the part after "+", or the legacy "@"). Non-timestamp suffixes such
// as the "backup-<sha>" form written by preserve-then-nuke share the same
// shape and are returned verbatim; only relative recency depends on this.
func branchRevision(branch, issueID string) string {
	idx := strings.Index(branch, "/"+issueID)
	if idx < 0 {
		return ""
	}
	tail := branch[idx+len(issueID)+1:]
	for _, sep := range []string{generatedIssueBranchSeparator, legacyIssueBranchSeparator} {
		if strings.HasPrefix(tail, sep) {
			return tail[len(sep):]
		}
	}
	return ""
}

// SurvivingBranchForIssue returns the most recently generated polecat branch
// on the rig's origin remote that encodes issueID, or "" when there is none.
// A non-empty error means the remote could not be queried; callers must fail
// open on it rather than treating the bead as feedable.
func SurvivingBranchForIssue(rigRoot, issueID string) (string, error) {
	branches, err := FindSurvivingBranchesForIssue(rigRoot, issueID)
	if err != nil {
		return "", err
	}
	if len(branches) == 0 {
		return "", nil
	}
	return branches[0], nil
}
