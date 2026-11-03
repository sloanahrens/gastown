package specdispatch

import (
	"fmt"
	"regexp"
	"strings"
)

// acceptanceBoxField is the field an acceptance-box refusal names: the failure
// is one criterion, not a template section.
const acceptanceBoxField = "acceptance"

// boxPhrase is one phrase that makes an acceptance item a step only the
// overseer can run. An item naming one holds a polecat to a check its sandbox
// cannot make (gt-hgt9j.3).
type boxPhrase struct {
	// text is the phrase searched for and the phrase a refusal names.
	text string
	// re is the matcher when a plain substring is too loose; nil means text
	// is matched case-insensitively as a substring.
	re *regexp.Regexp
}

// acceptanceBoxPhrases are read in order, so an item naming two steps is
// refused for the first one listed. The list is closed: a phrase outside it is
// not this check's (gt-hgt9j.3).
var acceptanceBoxPhrases = []boxPhrase{
	{text: "make gate"},
	// The boundary keeps a repository file named for the tool out: a
	// dockerfile is a file to change, not a container to run.
	{text: "docker", re: regexp.MustCompile(`(?i)\bdocker\b`)},
	{text: "kubectl"},
	{text: "colima"},
	{text: "live town"},
	{text: "the live"},
}

// matches reports whether item names the phrase.
func (p boxPhrase) matches(item string) bool {
	if p.re != nil {
		return p.re.MatchString(item)
	}
	return strings.Contains(strings.ToLower(item), p.text)
}

// acceptanceBoxPhrase returns the first phrase item names, and "" when it
// names none.
func acceptanceBoxPhrase(item string) string {
	for _, phrase := range acceptanceBoxPhrases {
		if phrase.matches(item) {
			return phrase.text
		}
	}
	return ""
}

// acceptanceBoxRefusal is the shape failure an acceptance text earns when one
// of its items names a step only the overseer can run: the first such item is
// refused with the phrase it matched, and the refusal says where the step
// belongs. The list is the field when it has one, else the description's
// acceptance section, so the field is the list a filer is held to (gt-hgt9j.3).
func acceptanceBoxRefusal(text string) (Refusal, bool) {
	for _, item := range acceptanceItemTexts(text) {
		phrase := acceptanceBoxPhrase(item)
		if phrase == "" {
			continue
		}
		return Refusal{
			Field:  acceptanceBoxField,
			Reason: fmt.Sprintf("item %q names %q, a step a polecat cannot run; the overseer-run steps belong in the Gate, as owed by the overseer", firstLine(item), phrase),
		}, true
	}
	return Refusal{}, false
}
