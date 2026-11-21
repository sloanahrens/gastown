package land

import (
	"strings"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// InsertFollowupLines adds add's lines after the body's last Goal bullet and
// its boxes after the last Acceptance box, and reports false with the body
// unchanged when there is no Goal bullet or no Acceptance box to place them by,
// or when the Acceptance section would hold more than specdispatch.MaxAcceptance
// boxes: a bundle never outgrows what one worker's landing carries, and the
// caller files a new bundle on false.
func InsertFollowupLines(body string, add FollowupAddition) (string, bool) {
	lines := strings.SplitAfter(body, "\n")
	section, goalAt, boxAt, boxes := "", -1, -1, 0
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(line, "\r\n"), "## "))
			continue
		}
		switch {
		case section == "Goal" && strings.HasPrefix(line, "- "):
			goalAt = i
		case section == "Acceptance" && strings.HasPrefix(line, "- [ ] "):
			boxAt = i
			boxes++
		}
	}
	if goalAt < 0 || boxAt < 0 || boxes+len(add.Boxes) > specdispatch.MaxAcceptance {
		return body, false
	}
	var b strings.Builder
	for i, line := range lines {
		b.WriteString(line)
		if i != goalAt && i != boxAt {
			continue
		}
		if !strings.HasSuffix(line, "\n") {
			b.WriteByte('\n')
		}
		added := add.Lines
		if i == boxAt {
			added = add.Boxes
		}
		for _, l := range added {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String(), true
}
