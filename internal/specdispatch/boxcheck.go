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

// boxPathRe is a source path inside an item: a separator token ending in a file
// extension with an optional line number, or a directory with a trailing slash,
// or a bare file name with an extension. A separator token with no extension,
// such as /usr/bin/kubectl, is a command and stays for the phrase search.
var boxPathRe = regexp.MustCompile(`[\w.-]*/[\w./-]*(?:\.[A-Za-z]\w*(?::\d+)?|/)|[\w-]+\.[A-Za-z]\w*(?::\d+)?`)

// findingInstructionRe marks where a finding item may start telling the worker
// to run something: a run verb or a shell marker.
var findingInstructionRe = regexp.MustCompile("(?i)\\b(?:run|execute|exec)\\b|[$`]")

// boxSubject is the part of item a phrase is searched in. An item opening with
// the finding prefix quotes the finding's title, which runs to the end of the
// item and cannot be told from an instruction written after it, so the title
// is skipped up to the first instruction marker and the rest is searched. Source
// paths are blanked wherever they sit, so a tool named inside a file path is not
// an instruction, while a command the item tells the worker to run, even by an
// absolute path, is still refused (gt-7c84xe).
func boxSubject(item string) string {
	if strings.HasPrefix(item, findingResolvedPrefix) {
		title := item[len(findingResolvedPrefix):]
		at := findingInstructionRe.FindStringIndex(title)
		if at == nil {
			return ""
		}
		item = title[at[0]:]
	}
	return boxPathRe.ReplaceAllString(item, " ")
}

// matches reports whether subject names the phrase.
func (p boxPhrase) matches(subject string) bool {
	if p.re != nil {
		return p.re.MatchString(subject)
	}
	return strings.Contains(strings.ToLower(subject), p.text)
}

// acceptanceBoxPhrase returns the first phrase item names outside its finding
// title and source paths, and "" when it names none.
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
