package dispatch

import (
	"regexp"
	"strings"
)

// SlingHookedMarker begins the line sling.Run prints once it has hooked the
// bead. The daemon's spec ticker counts it in the output of a tick killed
// before its report, so sling.Run prints the line from SlingHookedLine and the
// ticker counts with CountSlingHooked: a reworded line cannot drop out of the
// count unnoticed (gt-zeqqg).
const SlingHookedMarker = "Work attached to "

// slingHookedGlyph leads the hooked line, after its indent.
const slingHookedGlyph = "✓"

// ansiSGR matches the color sequences style.Bold.Render emits on a terminal.
var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// SlingHookedLine is the text sling.Run prints after the leading glyph, for
// the polecat it hooked the bead to.
func SlingHookedLine(polecat string) string {
	return SlingHookedMarker + polecat
}

// CountSlingHooked counts the hooked lines sling.Run printed in out. The prefix
// alone is not enough: `gt sling` and `gt hook` print other "Work attached to"
// lines ("Work attached to hook (status=hooked)", "Work attached to <agent>'s
// hook"). A line counts only when the glyph leads it and the polecat name is
// the single word after the marker, which those lines never are.
func CountSlingHooked(out []byte) int {
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(ansiSGR.ReplaceAllString(line, ""))
		rest, ok := strings.CutPrefix(line, slingHookedGlyph)
		if !ok {
			continue
		}
		rest, ok = strings.CutPrefix(strings.TrimSpace(rest), SlingHookedMarker)
		if ok && len(strings.Fields(rest)) == 1 {
			n++
		}
	}
	return n
}
