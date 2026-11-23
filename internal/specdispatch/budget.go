package specdispatch

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// changeBudgetGrammar is the line a bead's Size section states its change
// budget on (runaway beads 7/15, gt-xxd116). The words are matched
// case-insensitively; the full stop and the "in M files" clause are optional;
// N and M are positive integers that fit an int. The budget counts added
// lines, tests included: of the beads that fit one attempt, the median added
// 79 lines and the 75th percentile added 245.
const changeBudgetGrammar = "Change: at most N added lines in M files."

// changeBudgetPrefix starts a line that states a budget. It is lower case and
// matched case-insensitively with the grammar's words.
const changeBudgetPrefix = "change:"

// changeBudgetRe is changeBudgetGrammar as a regular expression over one
// trimmed line without its markdown decoration (budgetLineText).
// [1-9][0-9]* spells "positive integer", so a stated zero is a line that does
// not match rather than a budget of no lines.
var changeBudgetRe = regexp.MustCompile(`(?i)^change:\s+at\s+most\s+([1-9][0-9]*)\s+added\s+lines(?:\s+in\s+([1-9][0-9]*)\s+files)?\.?$`)

// declaredLineLimitRe matches every wording in which a bead declares its
// added-line limit to the done gate (gt-obyuam, gt-uq5b9u). It is copied
// verbatim from internal/done/linelimit.go, whose copy gt-yzhigm deletes
// (gt-hc43nx).
var declaredLineLimitRe = regexp.MustCompile(`(?i)\b(?:(?:about|under|at most|roughly|up to)\s+(\d+)\s+added lines?|(\d+)-added-line|(?:budget|limit|cap|stop)(?:\s+(?:is|of|at))?\s*(?::\s*|\s+)(\d+)\s+added lines?)\b`)

// budgetEmphasis removes the characters that mark emphasized text, so
// **bold**, __bold__ and *italic* read as the words they decorate.
var budgetEmphasis = strings.NewReplacer("*", "", "_", "")

// ChangeBudget is the change budget a bead's Size section states: at most
// Lines added lines across at most Files files. Files is 0 when the line
// leaves the files clause out: it then bounds the lines and not the files.
type ChangeBudget struct {
	Lines int
	Files int
}

// ParseChangeBudget reads a bead's Size section body, the "size" key of
// Sections(description), for its change budget line (changeBudgetGrammar):
// found is false when the section carries none, and a line that starts
// "Change:" without matching the grammar is an error quoting it. A line is read
// through the markdown a bead's prose carries — the leading bullet and the
// emphasis budgetLineText strips — so a bulleted or bolded budget is found
// rather than skipped as though the author had stated none. The first matching
// line answers; every "Change:" line must match. When no line starts "Change:",
// every wording DeclaredLineLimits reads is a declaration: the smallest number
// is the budget and Files is 0, so the lint reads what the gate refuses on.
func ParseChangeBudget(size string) (ChangeBudget, bool, error) {
	var budget ChangeBudget
	found := false
	for _, raw := range strings.Split(size, "\n") {
		line := strings.TrimSpace(raw)
		text := budgetLineText(line)
		if !strings.HasPrefix(strings.ToLower(text), changeBudgetPrefix) {
			continue
		}
		m := changeBudgetRe.FindStringSubmatch(text)
		if m == nil {
			return ChangeBudget{}, false, changeBudgetError(line)
		}
		if found {
			continue
		}
		lines, err := strconv.Atoi(m[1])
		if err != nil {
			return ChangeBudget{}, false, changeBudgetError(line)
		}
		budget.Lines = lines
		if m[2] != "" {
			files, err := strconv.Atoi(m[2])
			if err != nil {
				return ChangeBudget{}, false, changeBudgetError(line)
			}
			budget.Files = files
		}
		found = true
	}
	if !found {
		if limits := DeclaredLineLimits(size); len(limits) > 0 {
			budget.Lines = limits[0]
			for _, l := range limits[1:] {
				if l < budget.Lines {
					budget.Lines = l
				}
			}
			found = true
		}
	}
	return budget, found, nil
}

// DeclaredLineLimits returns the added-line limits a bead's text declares, in
// order of appearance and once each, or nil when it declares none.
func DeclaredLineLimits(text string) []int {
	var limits []int
	seen := make(map[int]bool)
	for _, m := range declaredLineLimitRe.FindAllStringSubmatch(text, -1) {
		digits := m[1]
		if digits == "" {
			digits = m[2]
		}
		if digits == "" {
			digits = m[3]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			// A count too large for an int is not a line limit a bead means,
			// and []int has no room to carry it, so the match states none.
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		limits = append(limits, n)
	}
	return limits
}

// budgetLineText returns a Size body line without the markdown that decorates
// it: the leading "- ", "* " or "+ " of a list bullet, and the * and _
// characters that mark emphasized text. A bullet marker counts only when a
// space follows it, as markdown writes one. The words, the numbers and the
// punctuation the grammar reads are left as written, so
// "- **Change:** at most 80 added lines." reads as the grammar's line. A
// decorated line that still does not match is reported by quoting the original
// line, never skipped as though the author had stated no budget.
func budgetLineText(line string) string {
	s := strings.TrimSpace(line)
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && (s[1] == ' ' || s[1] == '\t') {
		s = strings.TrimSpace(s[1:])
	}
	return budgetEmphasis.Replace(s)
}

// changeBudgetError refuses a stated budget the grammar cannot read. The
// message quotes the line and the grammar, so the author sees what was
// written next to what is expected.
func changeBudgetError(line string) error {
	return fmt.Errorf("Size section: %q does not match the change budget grammar %q", line, changeBudgetGrammar)
}
