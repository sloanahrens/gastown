package daemon

// The follow-up skip policy (gt-4qbe1w, Q34): the feed files beads for the
// minor findings of an approved landing, and a finding that asks for a doc or
// wording fix, or names a file no agent push may change, is a bead nobody can
// act on. The lessons hold-out withholds two classes for some beads instead, so
// the report can compare those findings with the ones filed (Q37). It lives
// here rather than in internal/land because it reads the lessons table, and
// omlessons imports land.

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	// followupReasonHoldOut marks a finding the lessons hold-out withholds for
	// this bead: it stays in the reviews log for the report to compare, and no
	// bead is filed for it (Q37).
	followupReasonHoldOut = "hold-out"

	// commentClaimClass is the omlessons class id for a comment that carries a
	// line number or an untrue claim (internal/omlessons/omlessons.go).
	commentClaimClass = "comment-claim"

	// protectedWorkflowDir is the repo directory whose files only a human may
	// change; the security model in docs/architecture.md says why.
	protectedWorkflowDir = ".forgejo/workflows"

	// followupTextSubject names a piece of text a wording nit can be about: a
	// comment, doc, documentation, message, log, log line or test name. The
	// ask patterns below and textSubject share the one list.
	followupTextSubject = `(?:comments?|docs?|documentation|messages?|logs?|log lines?|test names?)`
)

var (
	// mismatchWord matches a title that states a comment's claim or doc is
	// wrong, unverified or untested: the defect the skip policy exists to
	// file, so a title matching it is never a doc nit, whatever else it names
	// (gt-yzdg9r).
	mismatchWord = regexp.MustCompile(`(?i)\b(but|does not|doesn't|overstat\w*|misstat\w*|misdescrib\w*|wrong\w*|contradict\w*|assum\w*|unverif\w*|assert\w*|not exercised|not guarded|broader|claims?|says?|states?|ambiguous|inconsistent|gap)\b`)

	// commentWord matches a title that names a comment, the subject the
	// comment-claim class is about.
	commentWord = regexp.MustCompile(`(?i)\bcomments?\b`)

	// commentFormWord matches a title that asks a comment to change form only:
	// it narrates, restates or reflows, or cites a bead or rule number. A nit
	// the form list misses stays filed, because an unsure title is filed.
	commentFormWord = regexp.MustCompile(`(?i)\b(?:narrat\w*|restat\w*|re-?flow\w*|re-?wrap\w*|wrap(?:ped|s|ping)?|duplicat\w*|redundan\w*|histor\w*|sediment\w*|dangling|ungrammatical|bead citation|cites? a bead)\b|(?-i:\bR[0-9]+\b)`)

	// textSubject matches a piece of text a wording or spelling nit can name: a
	// comment, doc, message, log line or test name. A nit word on its own can
	// name a path spelling or a parser grammar, so the ask patterns below
	// require it beside this subject.
	textSubject = regexp.MustCompile(`(?i)\b` + followupTextSubject + `\b`)

	// proseNitAsk matches a title whose whole ask is the wording, phrasing or
	// rephrase of a named text: the prose word sits beside its subject, and
	// nothing follows but a path in parentheses. A prose word with no subject
	// beside it ("rephrase the refusal") does not match, so its finding is
	// filed (gt-yzdg9r).
	proseNitAsk = regexp.MustCompile(`(?i)^(?:[a-z]+\s+)?` + followupTextSubject + `\s+(?:wordings?|phrasings?|rephras\w*)\b(?:\s*\([^)]*\))?$|^(?:wordings?|phrasings?|rephras\w*)\s+(?:the\s+)?` + followupTextSubject + `\b(?:\s*\([^)]*\))?$|^(?:wordings?|phrasings?|rephras\w*)\s+(?:in|of|for|on)\s+(?:the\s+)?` + followupTextSubject + `\b(?:\s*\([^)]*\))?$`)

	// spellNitAsk matches a title whose whole ask is a typo, spelling or
	// grammar fix of a named text: the nit word sits beside its subject and
	// ends the ask, apart from a short qualifier ("for one day left") or a
	// path in parentheses. A title that states a result after the nit word
	// ("Grammar of the log filter rejects valid lines") does not match, so its
	// finding is filed (gt-yzdg9r).
	spellNitAsk = regexp.MustCompile(`(?i)^(?:[a-z]+\s+)?` + followupTextSubject + `\s+(?:typos?|spellings?|grammar)\b(?:\s+(?:for|in|on|of)\s+(?:\S+\s*){1,4})?(?:\s*\([^)]*\))?$|^(?:typos?|spellings?|grammar)\s+(?:in|of|for|on)\s+(?:the\s+)?` + followupTextSubject + `\b(?:\s*\([^)]*\))?$`)

	// spellLabel matches the label form "Typo: 'noul' used as a word": the nit
	// word opens the title and a colon introduces what to fix. isWordingNit
	// also requires a text subject in the title, so a defect labeled "Typo:"
	// with no text named is filed.
	spellLabel = regexp.MustCompile(`(?i)^(?:typos?|spellings?|grammar)\s*:`)
)

// followupPolicy is the skip policy and its clock. The clock is a field so the
// hold-out window can be tested without the wall clock (Q37).
type followupPolicy struct {
	now func() time.Time
}

// applyFollowupPolicy sets the follow-up skip policy on a Lander as it is built.
func applyFollowupPolicy(l *land.Lander) {
	p := followupPolicy{now: time.Now}
	l.FollowupSkip = p.reason
}

// reason is the skip policy: the reason a minor finding is filed nowhere, or
// "" to file it.
func (p followupPolicy) reason(w land.Work, c land.FollowupCandidate) string {
	if isProtectedFollowupPath(c.Path) {
		return followupReasonProtectedPath
	}
	if isDocNit(c) {
		return followupReasonDocNit
	}
	// The hold-out runs last, so a finding an earlier rule already skipped is
	// not counted as held out.
	if p.heldOut(w, c) {
		return followupReasonHoldOut
	}
	return ""
}

// heldOut reports whether the finding's class is withheld for the landing's
// bead at the policy's clock.
func (p followupPolicy) heldOut(w land.Work, c land.FollowupCandidate) bool {
	class := omlessons.Classify(land.ReviewFindingText{Title: c.Title, Claim: c.Claim})
	return omlessons.HeldOut(w.BeadID, class, p.now())
}

// isDocNit reports whether a finding asks for a doc or wording fix only. A
// title that states a mismatch is filed whatever else it names (gt-yzdg9r).
func isDocNit(c land.FollowupCandidate) bool {
	if mismatchWord.MatchString(c.Title) {
		return false
	}
	return isCommentFormNit(c) || isWordingNit(c.Title)
}

// isCommentFormNit reports whether a comment-claim finding asks the comment to
// change form. The comment-claim class also joins a finding on the claim phrase
// "the code does not", which a finding about missing behavior states too
// (gt-4qbe1w). A finding whose title never names a comment is about the code,
// so it stays filed; the class check runs first, so a finding an earlier class
// claims is never skipped here.
func isCommentFormNit(c land.FollowupCandidate) bool {
	return omlessons.Classify(land.ReviewFindingText{Title: c.Title, Claim: c.Claim}) == commentClaimClass &&
		commentWord.MatchString(c.Title) &&
		commentFormWord.MatchString(c.Title)
}

// isWordingNit reports whether a title's whole ask is the wording, typo,
// spelling or grammar fix of a named text. The nit word must sit beside its
// text subject and end the ask, so a title that names a path spelling, a
// parser grammar or a defect is filed (gt-yzdg9r).
func isWordingNit(title string) bool {
	return proseNitAsk.MatchString(title) ||
		spellNitAsk.MatchString(title) ||
		(spellLabel.MatchString(title) && textSubject.MatchString(title))
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
