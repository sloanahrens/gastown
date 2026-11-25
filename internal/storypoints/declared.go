package storypoints

import (
	"regexp"
	"strconv"
	"strings"
)

// declaredChangeRe is the Size section's change budget line, "Change: at
// most N added lines in M files.", over one line with its markdown decoration
// removed. N is a positive integer; the files clause is optional. It repeats
// specdispatch's changeBudgetRe so this reader accepts the lines the done gate
// reads, and it does not call specdispatch.
var declaredChangeRe = regexp.MustCompile(`(?i)^change:\s+at\s+most\s+([1-9][0-9]*)\s+added\s+lines(?:\s+in\s+[1-9][0-9]*\s+files)?\.?$`)

// declaredEmphasis removes the characters that mark emphasized text, so
// **Change:** reads as Change:.
var declaredEmphasis = strings.NewReplacer("*", "", "_", "")

// DeclaredLines returns the added-line count N that a bead's Size section
// declares on its "Change: at most N added lines in M files." line. ok is
// false when the Size section has no Change line, when a Change line does not
// match the grammar (N is not a positive integer), and when two Change lines
// state different numbers. Two lines with the same number declare one count.
func DeclaredLines(description string) (lines int, ok bool) {
	found := false
	for _, raw := range strings.Split(sizeSectionBody(description), "\n") {
		text := changeLineText(raw)
		if !strings.HasPrefix(strings.ToLower(text), "change:") {
			continue
		}
		m := declaredChangeRe.FindStringSubmatch(text)
		if m == nil {
			return 0, false
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		if found && n != lines {
			return 0, false
		}
		lines, found = n, true
	}
	return lines, found
}

// sizeSectionBody returns the body of the first "## Size" section of a bead's
// description, the lines up to the next "## " heading. It returns "" when the
// description has no Size section.
func sizeSectionBody(description string) string {
	var body []string
	in := false
	for _, line := range strings.Split(description, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "## ") {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(trimmed[len("## "):]), "size")
			continue
		}
		if in {
			body = append(body, line)
		}
	}
	return strings.Join(body, "\n")
}

// changeLineText returns a Size body line without the markdown that decorates
// it: the leading "- ", "* " or "+ " of a list bullet, and the * and _ marks of
// emphasis. A bullet marker counts only when a space follows it.
func changeLineText(line string) string {
	s := strings.TrimSpace(line)
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && (s[1] == ' ' || s[1] == '\t') {
		s = strings.TrimSpace(s[1:])
	}
	return declaredEmphasis.Replace(s)
}
