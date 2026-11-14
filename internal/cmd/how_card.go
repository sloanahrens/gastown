package cmd

import (
	_ "embed"
	"fmt"
	"strings"
	"unicode/utf8"
)

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
)

// overseerCardRules returns the card's rule lines, in file order. It has no
// caller in this tree yet: gt how --card (gt-4n47gt) is the next bead and reads
// the card through this function. A card that does not validate is an error,
// never an empty list.
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
