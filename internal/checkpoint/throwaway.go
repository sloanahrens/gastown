package checkpoint

import (
	"fmt"
	"path"
	"strings"
)

// Throwaway paths are files a polecat leaves in its worktree while poking at a
// live system and does not intend to commit: a diagnostic test run once and
// deleted, a /tmp copy, an editor backup, a patch leftover.
//
// They reach the target through the machine-generated commits rather than
// through the polecat. checkpoint_dog and gt done's gt-pvx safety net both run
// `git add -A` over a live worktree, and gt done collapses HEAD's TREE into the
// submitted commit, so a scratch file snapshotted between two dog cycles is
// submitted even after the polecat deletes it (gt-ozo4).
//
// The rule is an explicit ignore list rather than a heuristic, and it governs
// paths being ADDED to the repository: a modification to a file git already
// tracks is real work whatever its name, so a repository that legitimately
// tracks a matching name keeps working. Where the list cannot decide, callers
// leave the path out of the commit and report it — the safe direction is the
// one that cannot land content nobody asked for.

// throwawayBasenames are explicit throwaway file shapes, matched against a
// basename with path.Match (so `*` does not cross a separator).
var throwawayBasenames = []string{
	// Scratch/diagnostic source, including the gt-h1tq report's
	// internal/util/zz_livecheck_test.go.
	"zz_*",
	"*_zz_*",
	"*_livecheck*",
	// /tmp copies: `_tmp.` is deliberate — plain `*_tmp*` would also match
	// legitimate names like foo_tmpl.go.
	"*_tmp",
	"*_tmp.*",
	"*_tmp_*",
	"*.tmp",
	"*.temp",
	// Editor backups, swap files and patch leftovers.
	"*~",
	"*.bak",
	"*.orig",
	"*.rej",
	"*.swp",
	"*.swo",
	"*.swx",
	"*.swn",
	".#*",
	"#*#",
}

// throwawayDirSegments are directory names whose entire contents are scratch,
// matched against every segment of a repository-relative path.
var throwawayDirSegments = []string{"tmp", "scratch", "scratchpad"}

// IsThrowawayPath reports whether path — repository-relative and
// slash-separated, as git reports it — names a throwaway file that must never
// be committed on a polecat's behalf. Callers apply it to paths a commit would
// add, never to paths a repository already tracks.
func IsThrowawayPath(p string) bool {
	p = strings.TrimSpace(strings.TrimPrefix(p, "./"))
	if p == "" {
		return false
	}

	segments := strings.Split(p, "/")
	for _, segment := range segments[:len(segments)-1] {
		for _, dir := range throwawayDirSegments {
			if strings.EqualFold(segment, dir) {
				return true
			}
		}
	}

	base := segments[len(segments)-1]
	for _, pattern := range throwawayBasenames {
		if ok, err := path.Match(pattern, base); err == nil && ok {
			return true
		}
	}
	return false
}

// ThrowawayPaths filters paths down to the throwaway ones, preserving order and
// dropping duplicates. Callers pass repository-relative paths.
func ThrowawayPaths(paths []string) []string {
	seen := make(map[string]bool)
	var throwaway []string
	for _, p := range paths {
		if !IsThrowawayPath(p) || seen[p] {
			continue
		}
		seen[p] = true
		throwaway = append(throwaway, p)
	}
	return throwaway
}

// AddedThrowawayPaths reports the throwaway paths a submission of headRef would
// add to baseRef, or an error if that could not be determined.
//
// It answers about additions only, mirroring IsThrowawayPath: a file both refs
// already track is not something a submission brings. Everything above the
// merge base is what the branch itself contributes, including any commit
// checkpoint_dog wrote. An error means the question went unanswered, which a
// caller must not read as "nothing to report".
func AddedThrowawayPaths(workDir, baseRef, headRef string) ([]string, error) {
	mergeBase, err := gitOutput(workDir, "merge-base", baseRef, headRef)
	if err != nil {
		return nil, fmt.Errorf("finding merge-base of %s and %s: %w", baseRef, headRef, err)
	}

	// -z keeps a path with a newline in it from being split into two, so the
	// listing must be read verbatim.
	added, err := gitOutputRaw(workDir, "diff", "--name-only", "--diff-filter=A", "-z", mergeBase, headRef)
	if err != nil {
		return nil, fmt.Errorf("listing paths added by %s: %w", headRef, err)
	}

	return ThrowawayPaths(splitNullSeparated(added)), nil
}

// splitNullSeparated splits git's -z output, which terminates every record
// (including the last) with NUL, so the trailing empty chunk is not a path.
func splitNullSeparated(out string) []string {
	if out == "" {
		return nil
	}
	var paths []string
	for _, part := range strings.Split(out, "\x00") {
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths
}
