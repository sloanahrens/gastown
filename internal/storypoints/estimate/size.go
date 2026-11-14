package estimate

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// The size input (overseer trace 2026-11-14, docs/plans/2026-11-14-estimator-floor-trace.md,
// fix A). Shape, embed and jev are each calibrated to the middle of the landed
// beads and are averaged with equal weight, so a bead that asks for five
// one-line commands was rated 11 to 13 minutes and held under the split label
// whatever its size. When a bead's own text enumerates its work in a fenced
// list, the minutes that list implies cap the blend: they can lower an
// estimate and never raise one.
const (
	// sizeBaseMinutes and sizePerItemMinutes make a list of n items n*1.5+2
	// minutes: five items are 9.5.
	sizeBaseMinutes    = 2.0
	sizePerItemMinutes = 1.5
	// sizeMinItems is the shortest list that counts as an enumeration of work.
	sizeMinItems = 3
	// sizeMaxBoxes is the most acceptance boxes a bead that is sized by its
	// list may carry: a bead with more boxes has more to do than a list says.
	sizeMaxBoxes = 4
	// sizeFloorMinutes keeps a cap from reading a bead as trivial.
	sizeFloorMinutes = 4.0
)

// sizeMinutes is the size input of issue: the minutes its largest fenced list
// implies. ok is false when the bead has no list of at least sizeMinItems items
// or carries more than sizeMaxBoxes acceptance boxes, so the cap is a refusal
// to guess, never a default.
func sizeMinutes(issue *beads.Issue) (float64, bool) {
	items := largestFencedList(issue.Description)
	if items < sizeMinItems {
		return 0, false
	}
	boxes := countAcceptanceBoxes(issue.AcceptanceCriteria)
	if boxes == 0 {
		boxes = countAcceptanceBoxes(issue.Description)
	}
	if boxes > sizeMaxBoxes {
		return 0, false
	}
	m := sizeBaseMinutes + sizePerItemMinutes*float64(items)
	if m < sizeFloorMinutes {
		m = sizeFloorMinutes
	}
	return m, true
}

// largestFencedList is the number of non-blank lines in the largest fenced
// block (``` ... ```) of text; an unclosed block counts to the end of the text.
func largestFencedList(text string) int {
	largest, current := 0, 0
	inside := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inside && current > largest {
				largest = current
			}
			inside, current = !inside, 0
			continue
		}
		if inside && strings.TrimSpace(line) != "" {
			current++
		}
	}
	if inside && current > largest {
		largest = current
	}
	return largest
}

// countAcceptanceBoxes counts the checkbox lines ("- [ ]" or "- [x]") of text.
func countAcceptanceBoxes(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		t := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(t, "- [ ]") || strings.HasPrefix(t, "- [x]") {
			n++
		}
	}
	return n
}
