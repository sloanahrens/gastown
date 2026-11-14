package cmd

import (
	_ "embed"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// how_card.go holds the overseer card: the rule file and its loader
// (gt-j669b4.8), and the --card flag that prints it (gt-4n47gt).

// overseerCardFile holds the overseer card's rule lines, one per line and no
// headings: what a just-compacted overseer reads before it acts (gt-j669b4.8).
// The first lines are the design's first set (section 5), given verbatim by the
// overseer on that bead; the last three are the operator's standing rules
// (gt-0rix8g). TestHowCardRulesPinTheFile fails when a line changes, so an edit
// to the card is reviewed as a diff.
//
// It sits under howdata beside this package's code so the card is reviewed like
// code and deployed with the binary. It is not a file under directives/, which
// internal/config/directives.go lists as role directives and
// directives_deploy.go installs.
//
//go:embed howdata/overseer-card.md
var overseerCardFile string

const (
	// overseerCardPath names the embedded card in a validation error, so a
	// failing test points at the file to fix.
	overseerCardPath = "internal/cmd/howdata/overseer-card.md"
	// overseerCardMaxLine is the most characters one rule line may carry.
	overseerCardMaxLine = 200
	// overseerCardMaxBytes is the card's whole-file budget. The per-line
	// validator passes every line of a card far over this, so only a whole-file
	// check stops the card growing past what a compacted session will read
	// (gt-0rix8g).
	overseerCardMaxBytes = 2500
	// overseerCardTitle is the printed card's first line.
	overseerCardTitle = "Overseer card"
	// overseerCardLookupRule is the printed card's second line: how a task is
	// looked up before an operator step.
	overseerCardLookupRule = "before a roll, a resize, a release, a hand sling or any operator step: gt how <task> --history"
	// overseerCardPrintMaxBytes is the most the printed card may weigh. The
	// rule file has its own budget (overseerCardMaxBytes); this one bounds what
	// gt how --card writes, title, lookup rule and group lines included,
	// because that text is what reaches an agent's context.
	overseerCardPrintMaxBytes = 2500
)

// howCardFlag is --card: print the overseer card instead of the listing.
var howCardFlag bool

func init() {
	howCmd.Flags().BoolVar(&howCardFlag, "card", false, "print the overseer card: the lookup rule, the task groups and the rule lines")
}

// overseerCardRules returns the card's rule lines, in file order. howCardText,
// which gt how --card prints, reads the card through it. A card that does not
// validate is an error, never an empty list.
func overseerCardRules() ([]string, error) {
	return parseOverseerCard(overseerCardFile)
}

// parseOverseerCard splits text into rule lines and refuses a card that would
// read as broken: an empty line, a line over overseerCardMaxLine characters, or
// a card over overseerCardMaxBytes. The error names the file and, for a line,
// the 1-based number, so a test that trips it names what to fix, and it returns
// no lines, so a caller cannot print a card that failed validation.
func parseOverseerCard(text string) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		n := i + 1
		if strings.TrimSpace(line) == "" {
			return nil, fmt.Errorf("%s: line %d: empty line", overseerCardPath, n)
		}
		if c := utf8.RuneCountInString(line); c > overseerCardMaxLine {
			return nil, fmt.Errorf("%s: line %d: %d characters, more than %d", overseerCardPath, n, c, overseerCardMaxLine)
		}
	}
	if n := len(text); n > overseerCardMaxBytes {
		return nil, fmt.Errorf("%s: %d bytes, more than the %d-byte card budget", overseerCardPath, n, overseerCardMaxBytes)
	}
	return lines, nil
}

// runHowCard writes the overseer card to out. It refuses an argument: the card
// is one fixed text, and a group or word after --card would read as if it chose
// part of the card.
func runHowCard(out io.Writer, m howMap, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("gt how --card takes no group or word, got %q", strings.Join(args, " "))
	}
	card, err := howCardText(m)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, card)
	return err
}

// howCardText builds the card in one string: the title line, the lookup rule,
// every task group with its one-line description, then the rule lines. A group
// with no description (writeHowGroups) or a rule file that does not validate is
// an error, and so is a card over overseerCardPrintMaxBytes: a card that cannot
// be printed whole is not printed at all, so a failure writes nothing to out.
func howCardText(m howMap) (string, error) {
	rules, err := overseerCardRules()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n", overseerCardTitle, overseerCardLookupRule)
	if err := writeHowGroups(&b, m); err != nil {
		return "", err
	}
	b.WriteString("\n")
	for _, line := range rules {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if n := b.Len(); n >= overseerCardPrintMaxBytes {
		return "", fmt.Errorf("the overseer card is %d bytes, not under %d: trim the rule file or a group description", n, overseerCardPrintMaxBytes)
	}
	return b.String(), nil
}
