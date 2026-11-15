package fixrecurrence

import (
	"regexp"
	"strings"
)

// A description is read with the title classifier's tight rules only: the ones
// that put the cited id beside the signal phrase. The loose rules (still,
// again, after, since anywhere in a body) were right 8 times in 24 on titles,
// and a description is longer than a title, so they are left out here
// (gt-y3n4au).
const (
	// descriptionTitleConfidence is the confidence the fix-recurrence design
	// gives a title classifier's strict rule (gt-e9ur8q). Every rule here is
	// one of those; the loose rules are not.
	descriptionTitleConfidence = 0.9
	// descriptionScale weights a description against the title an agent writes
	// first: each rule here reports its title confidence times this.
	descriptionScale = 0.9
)

// descriptionRule is one tight rule: a pattern built around one cited id, the
// confidence its title counterpart carries, and the relationship it reports.
// The rules are tried in the order they are built, so a description that
// carries two signals is read as the first of them.
type descriptionRule struct {
	relationship    Relationship
	titleConfidence float64
	pattern         *regexp.Regexp
}

// match reports whether the rule fires for desc. Group 1 of every rule's
// pattern is the text just before the signal phrase: a negation cue there ("no
// recurrence", "no longer persists", "not the same class") turns the phrase
// into its opposite, and the rule does not fire.
func (r descriptionRule) match(desc string) bool {
	for _, m := range r.pattern.FindAllStringSubmatch(desc, -1) {
		if !negationCue.MatchString(m[1]) {
			return true
		}
	}
	return false
}

// negationCue matches a word that negates the signal phrase after it. It is
// read on the text immediately before the phrase, and up to four words may sit
// between the negator and the phrase ("not at all a recurrence of").
var negationCue = regexp.MustCompile(`(?i)\b(?:no|not|never|without|zero|isn['’]t|wasn['’]t|aren['’]t|doesn['’]t|didn['’]t|don['’]t)\s+(?:\w+\s+){0,4}$`)

// descriptionRules returns the tight rules for id, in priority order. Each
// pattern captures, as group 1, the text between the signal phrase and the id
// (or, for an id-first rule, between the id and the phrase).
func descriptionRules(id string) []descriptionRule {
	return []descriptionRule{
		// recurrence of <id>
		{Recurrence, descriptionTitleConfidence, regexp.MustCompile(`(?i)([^.!?\n]{0,60}?)\brecurrence of ` + descriptionIDEnd(id))},
		// <id> then the signal in the same sentence. A full stop after the id
		// ends the sentence there, so a signal that opens the next sentence
		// does not describe the id.
		{Recurrence, descriptionTitleConfidence, regexp.MustCompile(`(?i)` + descriptionIDSameSentence(id) + `[ \t]*([^.!?\n]{0,39}?)\b(?:recurrence|persists|not fixed)\b`)},
		// <id>'s fix incomplete | did not
		{Recurrence, descriptionTitleConfidence, regexp.MustCompile(`(?i)` + descriptionIDStart(id) + `['’]s fix ([^.!?\n]{0,20}?)(?:incomplete|did not)\b`)},
		// same class as <id>
		{SameClass, descriptionTitleConfidence, regexp.MustCompile(`(?i)([^.!?\n]{0,60}?)\bsame (?:bug )?class as ` + descriptionIDEnd(id))},
		// <id> regression | broke | introduced
		{Regression, descriptionTitleConfidence, regexp.MustCompile(`(?i)` + descriptionIDSameSentence(id) + `[ \t]*(?:\([0-9a-f]{6,12}\)[ \t]*)?([^.!?\n]{0,12}?)\b(?:regression|broke|introduced)\b`)},
		// <id> follow-up | hotfix | residual
		{FollowUp, descriptionTitleConfidence, regexp.MustCompile(`(?i)` + descriptionIDSameSentence(id) + `[ \t]*([^.!?\n]{0,12}?)\b(?:follow-up|hotfix|residual)\b`)},
		// follow-up to <id>
		{FollowUp, descriptionTitleConfidence, regexp.MustCompile(`(?i)([^.!?\n]{0,60}?)\bfollow-up to ` + descriptionIDEnd(id))},
	}
}

// descriptionIDStart matches the cited id where no id character runs into it
// from the left, and consumes the character before it.
func descriptionIDStart(id string) string {
	return `(?:^|[^a-z0-9.-])` + regexp.QuoteMeta(strings.ToLower(id))
}

// descriptionIDSameSentence matches the cited id at the start of a rule whose
// phrase follows it: no id character runs into it from either side, and the one
// character it consumes after the id may neither continue the id nor end its
// sentence or line: a full stop, an exclamation mark or a question mark ends the
// sentence there, so the
// rules built on this one read only what the id's own sentence says.
func descriptionIDSameSentence(id string) string {
	return descriptionIDStart(id) + `[^a-z0-9.!?\n\r-]`
}

// descriptionIDEnd matches the cited id up to its end: the character after it
// cannot continue an id, and a dot continues one only before a digit (a
// sub-bead id). It is for a rule whose own phrase already ends at a non-id
// character, which is the boundary the id needs on the left.
func descriptionIDEnd(id string) string {
	return regexp.QuoteMeta(strings.ToLower(id)) + `(?:[^a-z0-9.-]|\.(?:$|[^0-9])|$)`
}

// ClassifyDescription reads a bead's description against id, a closed bead it
// cites, using the tight rules only. It returns the first rule that matches,
// with that title rule's confidence times 0.9, or Reference and 0 when the
// description names the id only as background.
//
// An empty id classifies nothing: it is a Reference at 0 rather than a pattern
// that matches every sentence.
func ClassifyDescription(desc, id string) (Relationship, float64) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return Reference, 0
	}
	for _, r := range descriptionRules(id) {
		if r.match(desc) {
			return r.relationship, r.titleConfidence * descriptionScale
		}
	}
	return Reference, 0
}

// sharedTestIdentifier matches a test function's name: Test followed by an
// exported-style name, as a Go test is written.
var sharedTestIdentifier = regexp.MustCompile(`\bTest[A-Z]\w+`)

// SharedTestToken reports whether both titles name the same test function.
func SharedTestToken(titleA, titleB string) bool {
	first := sharedTestIdentifier.FindAllString(titleA, -1)
	if len(first) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(first))
	for _, name := range first {
		seen[name] = struct{}{}
	}
	for _, name := range sharedTestIdentifier.FindAllString(titleB, -1) {
		if _, ok := seen[name]; ok {
			return true
		}
	}
	return false
}
