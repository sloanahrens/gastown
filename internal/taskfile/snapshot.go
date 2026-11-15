package taskfile

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Brief is what a snapshot block shows about one bead: the fields a caller
// read out of the bead, since Snapshot itself reads no bead and no Dolt.
type Brief struct {
	ID          string
	Title       string
	Status      string
	Description string
}

const (
	// maxBriefKiB caps one bead's description at 2 KiB: the description is the
	// long field, and a task naming a dozen beads must still hand the seat a
	// file it can read whole.
	maxBriefKiB   = 2
	maxBriefBytes = maxBriefKiB * 1024

	// maxSnapshotKiB caps the whole snapshot at 24 KiB, well inside the
	// assembler's 64 KiB task text, so the snapshot leaves room for the task
	// around it.
	maxSnapshotKiB   = 24
	maxSnapshotBytes = maxSnapshotKiB * 1024
)

// briefTruncationMarker ends a description cut at maxBriefBytes. It reads the
// size from maxBriefKiB rather than writing it a second time, so the marker
// cannot name a cap the code no longer applies.
func briefTruncationMarker() string {
	return fmt.Sprintf("[description truncated: it exceeded %d KiB and the rest is not shown]", maxBriefKiB)
}

// snapshotTruncationMarker ends a snapshot cut at maxSnapshotBytes, for the
// same reason briefTruncationMarker reads its size from the constant.
func snapshotTruncationMarker() string {
	return fmt.Sprintf("[snapshot truncated: the beads exceeded %d KiB and the rest is not shown]", maxSnapshotKiB)
}

// beadIDPattern is the town's bead id shape: a two-to-four letter lowercase
// prefix, a hyphen, a three-to-six character lowercase core, then any dotted
// numeric child suffixes (gt-abc1, gt-abc1.12).
var beadIDPattern = regexp.MustCompile(`[a-z]{2,4}-[a-z0-9]{3,6}(?:\.[0-9]+)*`)

// Snapshot renders one block per bead that text names and lookup knows, in
// order of first appearance, and returns "" when lookup knows none of them.
//
// lookup is the only source of a Brief, so the same text and the same answers
// always render the same bytes: nothing here reads a clock, a bead or the
// environment. An id the lookup does not know is left out, and lookup is asked
// once per distinct id the text names, so a text naming no bead never calls it.
func Snapshot(text string, lookup func(id string) (Brief, bool)) string {
	var blocks []string
	for _, id := range beadIDs(text) {
		brief, ok := lookup(id)
		if !ok {
			continue
		}
		blocks = append(blocks, briefBlock(id, brief))
	}
	if len(blocks) == 0 {
		return ""
	}
	whole := "## Beads named in this task\n\n" + strings.Join(blocks, "\n\n") + "\n"
	return capBytes(whole, maxSnapshotBytes, snapshotTruncationMarker())
}

// beadIDs is each id named in text once, in order of first appearance. A
// candidate touching a glue byte is part of a longer token — the "wisp" of
// gt-wisp-yzuwzb — so it is dropped rather than offered to the lookup as a bead
// of its own.
func beadIDs(text string) []string {
	var ids []string
	seen := make(map[string]bool)
	for _, loc := range beadIDPattern.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && glueBefore(text[start-1]) {
			continue
		}
		if end < len(text) && glueAfter(text[end]) {
			continue
		}
		id := text[start:end]
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// glueBefore reports whether c can continue the token a candidate starts in. A
// dot counts here, so "x.gt-abc1" is one token rather than a bead after a
// sentence.
func glueBefore(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-' || c == '.'
}

// glueAfter reports whether c can continue the token a candidate ends in. A dot
// does not count here, so an id ending a sentence is still found.
func glueAfter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-'
}

// briefBlock is one bead's block: a heading naming it, its status and its
// description, each left out when the caller passed it empty so no label stands
// over nothing.
func briefBlock(id string, b Brief) string {
	heading := id
	if known := oneLine(b.ID); known != "" {
		heading = known
	}
	if title := oneLine(b.Title); title != "" {
		heading += ": " + title
	}
	var sb strings.Builder
	sb.WriteString("### " + heading)
	if status := oneLine(b.Status); status != "" {
		sb.WriteString("\n\nStatus: " + status)
	}
	if desc := strings.TrimRight(b.Description, "\n"); desc != "" {
		cut := capBytes(escapeLineHeadings(desc), maxBriefBytes, briefTruncationMarker())
		sb.WriteString("\n\n" + strings.TrimRight(cut, "\n"))
	}
	return sb.String()
}

// escapeLineHeadings stops a description from posing as structure: a line an
// ATX parser would read as a heading gets a backslash before its "#", so it
// renders as the text the bead wrote instead of forging a block heading of its
// own. Up to three leading spaces still start a heading; four spaces or a tab
// make indented code, which is already inert.
func escapeLineHeadings(text string) string {
	if !strings.Contains(text, "#") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		indent := 0
		for indent < len(line) && indent < 3 && line[indent] == ' ' {
			indent++
		}
		if indent < len(line) && line[indent] == '#' {
			lines[i] = line[:indent] + `\` + line[indent:]
		}
	}
	return strings.Join(lines, "\n")
}

// capBytes cuts text that exceeds limit bytes to fit it, marker included, and
// ends the result with marker on its own last line. It cuts at a rune boundary,
// so the result stays valid UTF-8 whatever a bead's description held, and a
// limit too small to hold the marker and two newlines panics on the slice: both
// callers pass a package constant an order of magnitude wider than its marker,
// so that is a code error and is not papered over.
func capBytes(text string, limit int, marker string) string {
	if len(text) <= limit {
		return text
	}
	cut := limit - len(marker) - 2
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimRight(text[:cut], "\n") + "\n" + marker + "\n"
}
