// Package fixrecurrence classifies the prose a bead's title or description
// already carries about an earlier fix, so the fix-recurrence signal reads what
// agents wrote instead of asking a model (gt-e9ur8q, gt-y3n4au).
package fixrecurrence

import (
	"regexp"
	"strings"
)

// Relationship is how a new bead's title relates to a closed bead it cites.
type Relationship string

const (
	// Recurrence says the symptom the cited bead fixed has come back.
	Recurrence Relationship = "recurrence"
	// SameClass says the new bead is another instance of the class the cited
	// bead fixed.
	SameClass Relationship = "same-class"
	// Regression says a change the cited bead made broke something.
	Regression Relationship = "regression"
	// FollowUp says the cited bead's fix was partial, or this work continues it.
	FollowUp Relationship = "follow-up"
	// Reference says the title cites the bead without claiming a relationship.
	Reference Relationship = "reference"
)

// The confidence a match carries: an exact phrasing ties its keyword to the id,
// while the weak rule only needs "again" or "still" near the id, and both words
// have meanings a title can use for something else entirely.
const (
	exactConfidence = 0.9
	looseConfidence = 0.7
)

// ClassifyTitle returns how title relates to the closed bead id and the
// confidence of the rule that decided it: 0.9 when a phrase ties a keyword to
// the id, 0.7 for the weak rule, and 0 for Reference, which is the answer when
// no rule matches, when the id is named only in passing, and when three or more
// ids compete for the title's claim.
func ClassifyTitle(title, id string) (Relationship, float64) {
	if id == "" {
		return Reference, 0
	}
	occurrences := idOccurrences(title, id)
	if len(occurrences) == 0 {
		return Reference, 0
	}
	// Three cited beads make it unclear which one a keyword belongs to, so the
	// title is a reference unless it names the grouping itself.
	if distinctIDCount(title) >= 3 && !groupingRe.MatchString(title) {
		return Reference, 0
	}
	for _, r := range rules {
		for _, occ := range occurrences {
			if r.matches(title, occ) {
				return r.relationship, r.confidence
			}
		}
	}
	return Reference, 0
}

// rule is one title phrasing: the relationship it claims, the confidence a
// match carries, and the test for the text around one citation of the id.
type rule struct {
	relationship Relationship
	confidence   float64
	matches      func(title string, occ idOccurrence) bool
}

// rules are tried in order and the first match anywhere in the title decides,
// which is the bead's own order with the weak rule last: an exact phrase beats
// the weak word, so "gt-prk broke the guard again" is the regression "broke"
// names rather than the recurrence "again" hints at.
var rules = []rule{
	{Recurrence, exactConfidence, matchesRecurrenceBefore},
	{Recurrence, exactConfidence, matchesRecurrenceAfter},
	{Recurrence, exactConfidence, matchesNeitherNor},
	{Recurrence, exactConfidence, matchesFixIncomplete},
	{SameClass, exactConfidence, matchesSameClass},
	{Regression, exactConfidence, matchesRegression},
	{FollowUp, exactConfidence, matchesFollowUp},
	{Recurrence, looseConfidence, matchesWeakRecurrence},
}

var (
	// The word for a symptom's return stands immediately before the id, joined
	// by at most "of" or "to", so the return is attributed to that bead:
	// "recurrence of gt-abc", "back after gt-abc".
	recurrenceBeforeRe = regexp.MustCompile(`(?i)\b(recurrence|recurs|resurfac\w*|back after)\b\s+(of\s+|to\s+)?$`)

	// The id comes first and the return word follows within 40 characters:
	// "gt-abc persists", "gt-abc is not fixed", "gt-abc will reopen". A word
	// family counts, so "persisted" and "reopened" are the same claim.
	recurrenceAfterRe = regexp.MustCompile(`(?is)^.{0,40}\b(recurrence|persist\w*|not fixed|reopen\w*)\b`)

	// "neither gt-abc" and "nor gt-abc" name the id as one of the beads the
	// claim covers, which is how a title says both are still failing.
	neitherNorRe = regexp.MustCompile(`(?i)\b(neither|nor)\s+$`)

	// "gt-abc's fix is incomplete", "gt-abc's fix only partly works",
	// "gt-abc's fix did not ...".
	fixIncompleteRe = regexp.MustCompile(`(?i)^['’]s fix\s+(is\s+)?(incomplete|only\s+partly|did\s+not)\b`)

	// "same bug class as gt-abc", within 40 characters on either side of the id.
	sameClassBeforeRe = regexp.MustCompile(`(?is)\bsame (bug )?class as\b.{0,40}$`)
	sameClassAfterRe  = regexp.MustCompile(`(?is)^.{0,40}\bsame (bug )?class as\b`)

	// "gt-abc (bb9c373) broke ...": the id, an optional short commit, then the
	// word naming what the change did.
	regressionAfterRe = regexp.MustCompile(`(?i)^\s*(\([0-9a-f]{7,40}\))?\s*(regression|broke|breaks|introduced|caused)\b`)

	// "caused by gt-abc", "unmasked by gt-abc", "regression gt-abc": the change
	// is named as what produced the id.
	regressionBeforeRe = regexp.MustCompile(`(?i)\b(regression|broke|caused by|introduced by|unmasked by)\s*$`)

	// "gt-abc follow-up", "gt-abc hotfix", "gt-abc residual": the work the id
	// left behind. "follow-up to gt-abc" says the same from the other side.
	followUpAfterRe  = regexp.MustCompile(`(?i)^\s*(follow-up|hotfix|residual|leftover|remaining)\b`)
	followUpBeforeRe = regexp.MustCompile(`(?i)\bfollow-up to\s*$`)

	// The weak rule: "again" or "still" within 80 characters after the id is
	// the vocabulary of a symptom that came back, and either word read alone is
	// too weak to carry the exact confidence.
	weakRecurrenceRe = regexp.MustCompile(`(?is)^.{0,80}\b(again|still)\b`)

	// groupingRe is the wording that lets a title naming several beads claim a
	// relationship anyway: "same class", "neither" or "nor".
	groupingRe = regexp.MustCompile(`(?i)\b(same (bug )?class|neither|nor)\b`)
)

func matchesRecurrenceBefore(title string, occ idOccurrence) bool {
	return recurrenceBeforeRe.MatchString(title[:occ.start])
}

func matchesRecurrenceAfter(title string, occ idOccurrence) bool {
	return recurrenceAfterRe.MatchString(clauseAfter(title[occ.end:]))
}

func matchesNeitherNor(title string, occ idOccurrence) bool {
	return neitherNorRe.MatchString(title[:occ.start])
}

func matchesFixIncomplete(title string, occ idOccurrence) bool {
	return fixIncompleteRe.MatchString(title[occ.end:])
}

func matchesSameClass(title string, occ idOccurrence) bool {
	return sameClassBeforeRe.MatchString(clauseBefore(title[:occ.start])) ||
		sameClassAfterRe.MatchString(clauseAfter(title[occ.end:]))
}

func matchesRegression(title string, occ idOccurrence) bool {
	return regressionAfterRe.MatchString(title[occ.end:]) ||
		regressionBeforeRe.MatchString(title[:occ.start])
}

func matchesFollowUp(title string, occ idOccurrence) bool {
	return followUpAfterRe.MatchString(title[occ.end:]) ||
		followUpBeforeRe.MatchString(title[:occ.start])
}

func matchesWeakRecurrence(title string, occ idOccurrence) bool {
	return weakRecurrenceRe.MatchString(clauseAfter(title[occ.end:]))
}

// clauseEnders are the characters that end a clause. A signal word past one
// belongs to a different claim: "gt-abc is fine; still the guard skips" names
// gt-abc as fine, so the "still" is not its recurrence.
const clauseEnders = ".!?;\n"

// clauseAfter is the part of text a rule may read after the cited id: it stops
// at the first clause end.
func clauseAfter(text string) string {
	if i := strings.IndexAny(text, clauseEnders); i >= 0 {
		return text[:i]
	}
	return text
}

// clauseBefore is the part of text a rule may read before the cited id: it
// starts after the last clause end, for the same reason.
func clauseBefore(text string) string {
	if i := strings.LastIndexAny(text, clauseEnders); i >= 0 {
		return text[i+1:]
	}
	return text
}

// idOccurrence is one place title names id: the byte offsets of the token.
type idOccurrence struct {
	start, end int
}

// idOccurrences is every whole-token citation of id in title, in order. A
// citation inside a longer id is not one: the gt-abc of gt-abc.12 is a
// different bead, so it is left to the id it is part of.
func idOccurrences(title, id string) []idOccurrence {
	if id == "" {
		return nil
	}
	var occurrences []idOccurrence
	for from := 0; from < len(title); {
		i := indexFold(title[from:], id)
		if i < 0 {
			break
		}
		start := from + i
		end := start + len(id)
		if continuesIDBefore(title, start) || continuesIDAfter(title, end) {
			from = start + 1
			continue
		}
		occurrences = append(occurrences, idOccurrence{start: start, end: end})
		from = end
	}
	return occurrences
}

// indexFold is the byte offset of the first case-insensitive match of needle in
// haystack, or -1. Bead ids are ASCII, so the byte-length slice EqualFold reads
// is the whole token on both sides.
func indexFold(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if strings.EqualFold(haystack[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// continuesIDBefore reports whether the byte before start is part of the same
// token, so the gt-abc of gt-abc.12 is not read as a citation of gt-abc.
func continuesIDBefore(title string, start int) bool {
	return start > 0 && (isIDByte(title[start-1]) || title[start-1] == '.')
}

// continuesIDAfter reports whether the byte at end continues the token. A dot
// continues it only when a digit follows, so an id ending a sentence is still a
// citation.
func continuesIDAfter(title string, end int) bool {
	if end >= len(title) {
		return false
	}
	c := title[end]
	if isIDByte(c) {
		return true
	}
	return c == '.' && end+1 < len(title) && title[end+1] >= '0' && title[end+1] <= '9'
}

func isIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-'
}

// beadIDPattern is the town's bead id shape as prose writes it, the same shape
// internal/taskfile reads: a two-to-four letter prefix, a hyphen, a
// three-to-six character core, then any dotted numeric child suffixes. Case is
// ignored because a citation is matched case-insensitively too.
var beadIDPattern = regexp.MustCompile(`(?i)[a-z]{2,4}-[a-z0-9]{3,6}(?:\.[0-9]+)*`)

// distinctIDCount is how many different beads title names. A candidate touching
// a longer token is part of it rather than an id of its own: the gt-wisp of
// gt-wisp-l8d8bv is not a bead.
func distinctIDCount(title string) int {
	seen := make(map[string]bool)
	for _, loc := range beadIDPattern.FindAllStringIndex(title, -1) {
		if continuesIDBefore(title, loc[0]) || continuesIDAfter(title, loc[1]) {
			continue
		}
		seen[strings.ToLower(title[loc[0]:loc[1]])] = true
	}
	return len(seen)
}
