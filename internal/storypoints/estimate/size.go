package estimate

import (
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// The size input (gt-uya91p): a bead that lists the commands or paths it asks
// for in a fenced list is no larger than that list says. It caps the blend, so
// it can lower an estimate and never raise one. The three averaged inputs are
// calibrated to the middle of the landed beads, so without it a five-command
// bead reads 11 to 13 minutes (docs/plans/2026-11-14-estimator-floor-trace.md).
const (
	// sizeBaseMinutes and sizePerItemMinutes make a list of n items n*1.5+2
	// minutes: five items are 9.5.
	sizeBaseMinutes    = 2.0
	sizePerItemMinutes = 1.5
	// sizeMinItems is the shortest list that counts as an enumeration of work.
	sizeMinItems = 3
	// sizeMaxBoxes is the most acceptance boxes a bead sized by its list may
	// carry: a bead with more boxes has more to do than a list says.
	sizeMaxBoxes = 4
)

// itemRE matches one line of a work list: a command of the town's tools or a
// repository path. A fenced block counts only when every non-blank line matches,
// so a code sample, a command's output or a table is never read as work.
var itemRE = regexp.MustCompile(`^((gt|bd|hm|om|go|make) [^\n]+|[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+/?)$`)

// sizeMinutes is the size input of issue: the minutes its largest fenced work
// list implies. ok is false when the bead has no such list of at least
// sizeMinItems items or carries more than sizeMaxBoxes acceptance boxes: the cap
// refuses to guess, it never defaults.
func sizeMinutes(issue *beads.Issue) (float64, bool) {
	items := largestWorkList(issue.Description)
	if items < sizeMinItems {
		return 0, false
	}
	boxes := countAcceptanceBoxes(issue.AcceptanceCriteria)
	if boxes == 0 {
		boxes = countAcceptanceBoxes(outsideFences(issue.Description))
	}
	if boxes > sizeMaxBoxes {
		return 0, false
	}
	return sizeBaseMinutes + sizePerItemMinutes*float64(items), true
}

// largestWorkList is the number of lines in the largest fenced block of text
// whose every non-blank line is a command or a path (itemRE). An unclosed block
// counts to the end of the text.
func largestWorkList(text string) int {
	largest, current := 0, 0
	inside, pure := false, true
	flush := func() {
		if pure && current > largest {
			largest = current
		}
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			if inside {
				flush()
			}
			inside, current, pure = !inside, 0, true
			continue
		}
		if !inside || t == "" {
			continue
		}
		if itemRE.MatchString(t) {
			current++
		} else {
			pure = false
		}
	}
	if inside {
		flush()
	}
	return largest
}

// outsideFences is text without the lines inside fenced blocks, so a checkbox
// quoted in a code sample is not an acceptance box.
func outsideFences(text string) string {
	var b strings.Builder
	inside := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inside = !inside
			continue
		}
		if !inside {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
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
