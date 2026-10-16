package land

import (
	"errors"
	"path"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
)

// RiskPathsFile is the repo-relative list of globs whose landings get
// gt:overseer-review-wanted. It is read from the landing's base commit, never
// the branch being landed, so a submission cannot unlist its own paths
// (gt-vsct7.4).
const RiskPathsFile = "internal/land/riskpaths.txt"

// reviewDeepLabel is the label the deep review puts on every bead it files for
// a finding (the deep-review-run formula's `-l review-deep`).
const reviewDeepLabel = "review-deep"

// reviewGeneratedLabels are the labels that make a bead review machinery's own
// output rather than work somebody filed: a finding a deep review raised, or
// one the post-landing om feed filed (FollowUpLabel).
var reviewGeneratedLabels = []string{reviewDeepLabel, FollowUpLabel}

// reviewGenerated returns the label on issue that makes its landing review
// machinery's own output, and whether issue wears one.
func reviewGenerated(issue *beads.Issue) (string, bool) {
	for _, label := range reviewGeneratedLabels {
		if beads.HasLabel(issue, label) {
			return label, true
		}
	}
	return "", false
}

// RiskPathsRecorded returns the risk paths a landing of issue records, and the
// label that made it record none.
//
// A bead the review machinery filed is that review's own output coming back
// around: the fix to a finding on a risk path is itself a risk-path landing,
// so it records no risk path and takes no gt:overseer-review-wanted. Neither
// the attention queue's landing.risk_path item nor the deep review's landings
// mode picks it up, while om still gates it, the record's base and head still
// name the change, and the bead's note says the review was skipped (gt-g349w).
func RiskPathsRecorded(issue *beads.Issue, paths []string) (recorded []string, skippedBy string) {
	if len(paths) == 0 {
		return paths, ""
	}
	if label, ok := reviewGenerated(issue); ok {
		return nil, label
	}
	return paths, ""
}

// RiskPaths returns the paths base..head changes that match a glob in
// RiskPathsFile at base, sorted. It never rejects or delays a landing: it
// answers what a landing touched, and the label built on it is advisory.
//
// An empty list means either no changed path matched or the file is absent at
// base; a git failure is returned, and the caller decides to land unlabelled.
func RiskPaths(g Repo, base, head string) ([]string, error) {
	globs, err := RiskPathGlobs(g, base)
	if err != nil {
		return nil, err
	}
	if len(globs) == 0 {
		return nil, nil
	}
	changed, err := g.DiffNameOnly(base, head)
	if err != nil {
		return nil, err
	}
	return MatchRiskPaths(globs, changed), nil
}

// RiskPathGlobs reads the globs from RiskPathsFile at ref. A file absent at
// ref is no globs and no error: a repo that has not landed this slice yet
// labels nothing.
func RiskPathGlobs(g Repo, ref string) ([]string, error) {
	text, err := g.ShowFileAtRev(ref, RiskPathsFile)
	if errors.Is(err, git.ErrNotAtRef) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseRiskPathGlobs(text), nil
}

// ParseRiskPathGlobs splits the list into the globs it names: one per line,
// with blank lines and # comments ignored.
func ParseRiskPathGlobs(text string) []string {
	var globs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		globs = append(globs, line)
	}
	return globs
}

// MatchRiskPaths returns the changed paths that match any glob, sorted and
// without duplicates.
func MatchRiskPaths(globs, changed []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range changed {
		if seen[p] {
			continue
		}
		for _, glob := range globs {
			if matchRiskGlob(glob, p) {
				seen[p] = true
				out = append(out, p)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// matchRiskGlob reports whether one path matches one glob. Matching is
// path.Match per segment, so '*' stops at a separator, with one extension: a
// trailing /** matches the whole subtree under the leading segments
// ("internal/daemon/**" matches internal/daemon/anything/deep.go). A '**'
// anywhere else is a plain per-segment '*' and will not cross a separator, so
// "internal/**/x.go" does not reach a nested path; write the subtree form.
//
// A malformed glob matches nothing rather than failing: the list is read from
// a landed commit, and a typo in it must not stop a landing.
func matchRiskGlob(glob, p string) bool {
	segments := strings.Split(path.Clean(glob), "/")
	target := strings.Split(path.Clean(p), "/")
	if last := len(segments) - 1; last >= 0 && segments[last] == "**" {
		prefix := segments[:last]
		if len(target) < len(prefix) {
			return false
		}
		return matchSegments(prefix, target[:len(prefix)])
	}
	if len(segments) != len(target) {
		return false
	}
	return matchSegments(segments, target)
}

// matchSegments matches two equal-length segment lists with path.Match. A
// malformed pattern segment matches nothing.
func matchSegments(pattern, target []string) bool {
	for i, seg := range pattern {
		ok, err := path.Match(seg, target[i])
		if err != nil || !ok {
			return false
		}
	}
	return true
}
