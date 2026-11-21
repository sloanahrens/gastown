package done

import (
	"fmt"
	"io"
	"strings"
)

// noLimitLine is what the check prints when the bead declares no added-line
// limit, so the skipped check is visible rather than silent (gt-wxpd0y).
const noLimitLine = "no limit declared in the bead's Size or Constraints: the added-line check did not run"

// splitLine is what a refusal past twice the limit tells the seat to do
// instead of trimming (gt-wxpd0y): push the commit first so it outlives the
// seat and a split can be built from it (gt-574kps).
const splitLine = "This needs a split, not a trim: push the branch first with a plain git push origin HEAD:refs/heads/<your branch> (never --force), read git ls-remote origin <your branch> and check the tip is your HEAD, comment both shas on the bead, then comment that the bead needs a split and stop; do not run gt done again with the same change."

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

// sizeRefusalError is the refusal of a polecat past twice the declared limit,
// apart from the unreadable added-line count that also stops a run: a caller
// that acts on the refusal alone asks errors.As for it.
type sizeRefusalError string

func (e sizeRefusalError) Error() string { return string(e) }

// crewWarningPrefix marks a crew's over-limit lines apart from a polecat refusal (gt-zqg4m2).
const crewWarningPrefix = "warning (crew branch, not refused): "

// reportLineLimit prints the added-line check's verdict and returns the refusal
// for a polecat past twice the declared limit; a crew branch is warned on errOut
// instead, an unreadable count is an error, and no declared limit skips the count.
func reportLineLimit(out, errOut io.Writer, count func() (int, error), description string, polecat bool) error {
	limits := DeclaredLineLimits(description)
	if len(limits) == 0 {
		_, lines := lineLimitVerdict(limits, 0)
		fmt.Fprintln(out, lines[0])
		return nil
	}
	added, err := count()
	if err != nil {
		return fmt.Errorf("added-line check: %w", err)
	}
	refuse, lines := lineLimitVerdict(limits, added)
	if refuse && !polecat {
		for _, line := range lines {
			fmt.Fprintln(errOut, crewWarningPrefix+line)
		}
		return nil
	}
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
	if refuse {
		return sizeRefusalError(strings.Join(lines, "\n"))
	}
	return nil
}
