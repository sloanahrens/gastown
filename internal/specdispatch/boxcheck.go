package specdispatch

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
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

// findingResolvedPrefix opens an acceptance item that names a finding. It is
// the prefix internal/land writes (findingResolved there); specdispatch cannot
// import land, so the text is repeated here.
const findingResolvedPrefix = "Finding resolved: "

// boxPathPattern is a source path inside an item: a separator token ending in a
// file extension with an optional line number, or a directory with a trailing
// slash, or a bare file name with an extension. A separator token with no
// extension, such as /usr/bin/kubectl, is a command and stays for the phrase
// search.
const boxPathPattern = `[\w.-]*/[\w./-]*(?:\.[A-Za-z]\w*(?::\d+)?|/)|[\w-]+\.[A-Za-z]\w*(?::\d+)?`

var boxPathRe = regexp.MustCompile(boxPathPattern)

// boxTrailingPathRe is the "(path)" group that ends a finding item, where the
// finding names its source location.
var boxTrailingPathRe = regexp.MustCompile(`\s*\((?:` + boxPathPattern + `)\)$`)

// findingClauseBreaks are the characters that make a finding title more than one
// clause: a sentence or list stop, a clause separator, a shell marker, or a line
// break.
const findingClauseBreaks = ".;:`$\n"

// boxSubject is the part of item a phrase is searched in. An item opening with
// the finding prefix quotes the finding's title: when the title is one plain
// clause (no findingClauseBreaks character), a tool name in it or in the
// trailing "(path)" group is not an instruction, so the subject is empty. Any
// other finding item is searched whole, less its trailing group, so an
// instruction written as a second clause is refused, with or without a run
// verb. Items without the prefix have their source paths blanked wherever they
// sit, so a tool named in a path is not an instruction, while a command the
// item tells the worker to run, even by an absolute path, is still refused.
//
// Accepted residual: an instruction written with no punctuation after the
// prefix reads as a plain title and is not detected. The title is free text
// from the finding, so no word list can tell it from an instruction (gt-7c84xe).
func boxSubject(item string) string {
	if !strings.HasPrefix(item, findingResolvedPrefix) {
		return boxPathRe.ReplaceAllString(item, " ")
	}
	title := item[len(findingResolvedPrefix):]
	if loc := boxTrailingPathRe.FindStringIndex(title); loc != nil {
		title = title[:loc[0]]
	}
	if !strings.ContainsAny(title, findingClauseBreaks) {
		return ""
	}
	return findingResolvedPrefix + title
}

// matches reports whether subject names the phrase.
func (p boxPhrase) matches(subject string) bool {
	if p.re != nil {
		return p.re.MatchString(subject)
	}
	return strings.Contains(strings.ToLower(subject), p.text)
}

// acceptanceBoxPhrase returns the first phrase in the item's subject (see
// boxSubject), and "" when the subject names none.
func acceptanceBoxPhrase(item string) string {
	subject := boxSubject(item)
	for _, phrase := range acceptanceBoxPhrases {
		if phrase.matches(subject) {
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

// nestedAcceptanceBoxRefusal is acceptanceBoxRefusal for the nested items the
// top-level scan skips. A sub-step under a criterion is a step the polecat runs
// too, so an offending one is refused the same way. The nested lines are
// rejoined unindented, so the one scan reads them and words the refusal
// (gt-hgt9j.3).
func nestedAcceptanceBoxRefusal(text string) (Refusal, bool) {
	var nested []string
	for _, item := range beads.AcceptanceItems(text) {
		if item.Nested {
			nested = append(nested, item.Line)
		}
	}
	return acceptanceBoxRefusal(strings.Join(nested, "\n"))
}
