package done

import (
	"fmt"
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
