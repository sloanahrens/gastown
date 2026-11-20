package specdispatch

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// evidenceSection is the "## " heading, lower-cased as Sections keys it, where
// a bead pastes the command it ran on a fixture and the tail of its output.
const evidenceSection = "evidence"

// evidenceReason is the refusal's fix, with the fixture path filled in: the
// claim is checked by running the rule on that fixture and showing the result.
const evidenceReason = "the acceptance claims a zero result on %s: run the rule on that fixture and paste the command and the output tail under ## Evidence"

// zeroClaimRes are the shapes a zero-result claim takes in one acceptance line
// (gt-s7suwn). Each is read case-insensitively: the phrases the rule names, and
// the "no <words> on" form, which runs to the end of its clause.
var zeroClaimRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bzero findings\b`),
	regexp.MustCompile(`(?i)\bnone of\b`),
	regexp.MustCompile(`(?i)\bno\b[^,.;]*?\bon\b`),
	regexp.MustCompile(`(?i)\bpasses on\b`),
}

// evidenceRefusal refuses a bead whose acceptance claims a zero result on a
// fixture path and shows no usable "## Evidence" section, since nothing at
// landing can check the claim. It reads no CreatedAt: every bead is checked,
// and the grandfather date is a later bead's rule (gt-qhrsa4). Whether an empty
// fenced block counts as usable is a later bead's rule too (gt-afagyn).
func evidenceRefusal(s Spec, sections map[string]string) (Refusal, bool) {
	if usableEvidence(sections[evidenceSection]) {
		return Refusal{}, false
	}
	for _, line := range strings.Split(acceptanceText(s, sections), "\n") {
		fixture := fixturePathOn(line)
		if fixture == "" || !hasZeroClaim(line) {
			continue
		}
		return Refusal{Field: "## Evidence", Reason: fmt.Sprintf(evidenceReason, fixture)}, true
	}
	return Refusal{}, false
}

// usableEvidence reports whether an Evidence section shows a command: a line
// starting "$ ", or an opening fence and its closer.
func usableEvidence(section string) bool {
	var fence byte
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "$ ") {
			return true
		}
		marker := beads.FenceMarker(trimmed)
		if marker == "" {
			continue
		}
		if fence == 0 {
			fence = marker[0]
			continue
		}
		if marker[0] == fence {
			return true
		}
	}
	return false
}

// hasZeroClaim reports whether a line states a zero result.
func hasZeroClaim(line string) bool {
	for _, re := range zeroClaimRes {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// fixturePathOn returns the first fixture path named on a line, or "". It reads
// the line's tokens through PathTokens, the same reader the risk-path warning
// uses, so the two spellings of a path agree.
func fixturePathOn(line string) string {
	for _, token := range PathTokens(line) {
		if isFixturePath(token) {
			return token
		}
	}
	return ""
}

// isFixturePath reports whether p is a fixture path: one with a testdata/
// directory under internal/, one under deploy/, or any path whose basename
// names a fixture and carries an extension. The basename rule is checked last
// and covers an internal/ path with no testdata segment, so no branch above it
// may answer false for an internal/ path on its own.
func isFixturePath(p string) bool {
	switch {
	case p == "":
		return false
	case strings.HasPrefix(p, "deploy/"):
		return true
	case strings.HasPrefix(p, "internal/") && hasSegment(p, "testdata"):
		return true
	}
	return isFixtureFile(p)
}

// hasSegment reports whether a slash-separated path holds segment exactly.
func hasSegment(p, segment string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == segment {
			return true
		}
	}
	return false
}

// isFixtureFile reports whether a path's basename names a fixture and carries
// an extension: fixture_ok.json yes, fixtures/ no.
func isFixtureFile(p string) bool {
	base := path.Base(p)
	ext := path.Ext(base)
	return ext != "" && strings.TrimSuffix(base, ext) != "" &&
		strings.Contains(strings.ToLower(base), "fixture")
}
