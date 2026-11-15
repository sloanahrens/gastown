// Package fixrecurrence reads the prose in which agents already write that a
// bead is a recurrence of a fix that did not hold (gt-y3n4au).
package fixrecurrence

import (
	"regexp"
	"strings"
)

// Relationship is how a new bead stands to a closed bead it cites.
type Relationship string

// The relationships a cited bead can hold. Reference is the answer for a bead
// that merely mentions the id: no signal phrase sits beside it.
const (
	// Recurrence is the symptom back after a fix: the fix did not hold.
	Recurrence Relationship = "recurrence"
	// SameClass is another instance of the bug the cited fix closed.
	SameClass Relationship = "same class"
	// Regression is a defect the cited fix introduced.
	Regression Relationship = "regression"
	// FollowUp is the residue of a fix that was partial.
	FollowUp Relationship = "follow-up"
	// Reference is a mention of the id with no signal beside it.
	Reference Relationship = "reference"
)

// A description is read with the title classifier's tight rules only: the ones
// that put the cited id beside the signal phrase. The loose rules (still,
// again, after, since anywhere in a body) were right 8 times in 24 on titles,
// and a description is longer than a title, so they are left out here
// (gt-y3n4au).
const (
	// descriptionTightConfidence is a title exact rule's confidence (0.9) times
	// the description scale (0.9). A description is a weaker signal than the
	// title an agent writes first, so every tight rule here carries this
	// confidence.
	descriptionTightConfidence = 0.81
)

// descriptionRule is one tight rule: a pattern built around one cited id, and
// the relationship it reports. The rules are tried in the order they are built,
// so a description that carries two signals is read as the first of them.
type descriptionRule struct {
	relationship Relationship
	pattern      *regexp.Regexp
}

// descriptionRules returns the tight rules for id, in priority order.
func descriptionRules(id string) []descriptionRule {
	return []descriptionRule{
		// recurrence of <id>
		{Recurrence, regexp.MustCompile(`(?i)\brecurrence of ` + descriptionIDEnd(id))},
		// <id> then the signal within 40 characters
		{Recurrence, regexp.MustCompile(`(?is)` + descriptionIDWhole(id) + `.{0,39}?(?:recurrence|persists|not fixed)`)},
		// <id>'s fix incomplete | did not
		{Recurrence, regexp.MustCompile(`(?is)` + descriptionIDStart(id) + `['’]s fix (?:is )?(?:incomplete|did not)`)},
		// same class as <id>
		{SameClass, regexp.MustCompile(`(?i)\bsame (?:bug )?class as ` + descriptionIDEnd(id))},
		// <id> regression | broke | introduced
		{Regression, regexp.MustCompile(`(?is)` + descriptionIDWhole(id) + `\s*(?:\([0-9a-f]{6,12}\)\s*)?(?:regression|broke|introduced)\b`)},
		// <id> follow-up | hotfix | residual
		{FollowUp, regexp.MustCompile(`(?is)` + descriptionIDWhole(id) + `\s*(?:follow-up|hotfix|residual)\b`)},
		// follow-up to <id>
		{FollowUp, regexp.MustCompile(`(?i)\bfollow-up to ` + descriptionIDEnd(id))},
	}
}

// descriptionIDStart matches the cited id where no letter or digit runs into it
// from the left, and consumes the character before it.
func descriptionIDStart(id string) string {
	return `(?:^|[^a-z0-9.])` + regexp.QuoteMeta(strings.ToLower(id))
}

// descriptionIDWhole matches the cited id as a whole token: no letter or digit
// runs into it from either side, and a dot continues it only before a digit (a
// sub-bead id). It consumes the character on each side.
func descriptionIDWhole(id string) string {
	return descriptionIDStart(id) + `(?:[^a-z0-9.]|\.(?:$|[^0-9])|$)`
}

// descriptionIDEnd matches the cited id up to its end: the character after it
// cannot continue an id, and a dot continues one only before a digit (a
// sub-bead id). It is for a rule whose own phrase already ends at a non-id
// character, which is the boundary the id needs on the left.
func descriptionIDEnd(id string) string {
	return regexp.QuoteMeta(strings.ToLower(id)) + `(?:[^a-z0-9.]|\.(?:$|[^0-9])|$)`
}

// ClassifyDescription reads a bead's description against id, a closed bead it
// cites, using the tight rules only. It returns the first rule that matches,
// with 0.9 times the title rule's confidence, or Reference and 0 when the
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
		if r.pattern.MatchString(desc) {
			return r.relationship, descriptionTightConfidence
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
