package daemon

// The follow-up skip policy (gt-4qbe1w, Q34): the feed files beads for the
// minor findings of an approved landing, and a finding that asks for a doc or
// wording fix, or names a file no agent push may change, is a bead nobody can
// act on. It lives here rather than in internal/land because it reads the
// lessons table, and omlessons imports land.

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

const (
	// followupReasonDocNit marks a finding that asks for a doc or wording fix:
	// a nit, not a defect to file.
	followupReasonDocNit = "doc-nit"
	// followupReasonProtectedPath marks a finding about a file under
	// protectedWorkflowDir, which no agent push may change.
	followupReasonProtectedPath = "protected-path"

	// commentClaimClass is the omlessons class id for a comment that carries a
	// line number or an untrue claim (internal/omlessons/omlessons.go).
	commentClaimClass = "comment-claim"

	// protectedWorkflowDir is the repo directory whose files only a human may
	// change; the security model in docs/architecture.md says why.
	protectedWorkflowDir = ".forgejo/workflows"
)

var (
	// wordingNitPattern matches a title that asks for a typo, spelling, grammar
	// or wording fix. Each alternative is a whole word, so a real defect whose
	// title carries one as a fragment ("misspelling") is not a nit.
	wordingNitPattern = regexp.MustCompile(`(?i)\b(typos?|spellings?|grammar|wordings?|phrasings?|rephras\w*)\b`)

	// commentWord matches a title that names a comment, the subject the
	// comment-claim class is about.
	commentWord = regexp.MustCompile(`(?i)\bcomments?\b`)
)

// applyFollowupPolicy sets the follow-up skip policy on a Lander as it is built.
func applyFollowupPolicy(l *land.Lander) {
	l.FollowupSkip = followupSkipReason
}

// followupSkipReason is the skip policy: the reason a minor finding is filed
// nowhere, or "" to file it.
func followupSkipReason(_ land.Work, c land.FollowupCandidate) string {
	if isProtectedFollowupPath(c.Path) {
		return followupReasonProtectedPath
	}
	if isDocNit(c) {
		return followupReasonDocNit
	}
	return ""
}

// isDocNit reports whether a finding asks for a doc or wording fix.
func isDocNit(c land.FollowupCandidate) bool {
	if wordingNitPattern.MatchString(c.Title) {
		return true
	}
	// The comment-claim class also joins a finding on the claim phrase "the
	// code does not", which a finding about missing behavior states too
	// (gt-4qbe1w). A finding whose title never names a comment is about the
	// code, so it stays filed; the class check runs first, so a finding an
	// earlier class claims is never skipped here.
	return omlessons.Classify(land.ReviewFindingText{Title: c.Title, Claim: c.Claim}) == commentClaimClass &&
		commentWord.MatchString(c.Title)
}

// isProtectedFollowupPath reports whether p names a file under the repository's
// workflow directory. The directory is matched at a segment boundary anywhere
// in the path, so a path recorded from a checkout counts with or without its
// root; a path that climbs out of the tree ("..") names no repository file and
// does not count.
func isProtectedFollowupPath(p string) bool {
	p = path.Clean(filepath.ToSlash(strings.TrimSpace(p)))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	return strings.Contains("/"+p+"/", "/"+protectedWorkflowDir+"/")
}
