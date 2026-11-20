package done

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// noLimitLine is what the check prints when the bead declares no added-line
// limit, so the skipped check is visible rather than silent (gt-wxpd0y).
const noLimitLine = "no limit declared in the bead's Size or Constraints: the added-line check did not run"

// splitLine is what a refusal past twice the limit tells the seat to do
// instead of trimming (gt-wxpd0y).
const splitLine = "This needs a split, not a trim: comment on the bead that it needs a split and stop; do not run gt done again with the same change."

// lineLimitVerdict decides whether added is too many for the limit the bead
// declares, checked at the largest of several numbers, and returns the lines
// to print: the refusal past twice the limit, a warning past the limit, or the
// no-limit line (gt-wxpd0y).
func lineLimitVerdict(limits []int, added int) (refuse bool, lines []string) {
	if len(limits) == 0 {
		return false, []string{noLimitLine}
	}
	limit := limits[0]
	for _, l := range limits[1:] {
		if l > limit {
			limit = l
		}
	}
	stated := ""
	if len(limits) > 1 {
		parts := make([]string, len(limits))
		for i, l := range limits {
			parts[i] = fmt.Sprint(l)
		}
		stated = "the bead states " + strings.Join(parts, " and ") + "; "
	}
	line := fmt.Sprintf("Change exceeds the %d-added-line constraint: %d added lines against %d declared (%sthe refusal is at twice the limit).",
		limit, added, limit, stated)
	if added > 2*limit {
		return true, []string{line, splitLine}
	}
	if added > limit {
		return false, []string{line}
	}
	return false, nil
}

// lineLimitOut is where reportLineLimit prints its verdict lines; a test
// replaces it to read them (gt-zqg4m2).
var lineLimitOut io.Writer = os.Stdout

// reportLineLimit prints the added-line check's verdict and, for a polecat
// past twice the declared limit, returns the refusal; a crew branch is only
// warned, and a count that cannot be read is an error (gt-zqg4m2). With no
// limit declared the count is not read, so the no-limit line is printed.
func reportLineLimit(count func() (int, error), description string, polecat bool) error {
	limits := DeclaredLineLimits(description)
	if len(limits) == 0 {
		_, lines := lineLimitVerdict(limits, 0)
		fmt.Fprintln(lineLimitOut, lines[0])
		return nil
	}
	added, err := count()
	if err != nil {
		return fmt.Errorf("added-line check: %w", err)
	}
	refuse, lines := lineLimitVerdict(limits, added)
	for _, line := range lines {
		fmt.Fprintln(lineLimitOut, line)
	}
	if refuse && polecat {
		return errors.New(strings.Join(lines, "\n"))
	}
	return nil
}
