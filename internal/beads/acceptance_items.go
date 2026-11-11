package beads

import (
	"errors"
	"regexp"
	"strings"
)

// acceptanceItemRe matches a list item: a "-", "*" or "+" bullet, or a "1."
// or "1)" number, then its text. Group 1 is the text after the marker.
var acceptanceItemRe = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(\S.*)$`)

// acceptanceBoxRe matches the checkbox that opens an item's text.
var acceptanceBoxRe = regexp.MustCompile(`^\[([ xX])\]`)

// AcceptanceItem is one list item of acceptance text. The spec lint counts
// items and the close gates count unchecked boxes, so both read this one
// matcher and agree on what a criterion is.
type AcceptanceItem struct {
	Line    string // the item's line, trimmed of surrounding space
	Nested  bool   // indented under another item
	Boxed   bool   // the text opens with a "[ ]" or "[x]" checkbox
	Checked bool   // the box is "[x]" or "[X]"
}

// AcceptanceItems returns the list items of text, in order. A fenced code
// block is example text, so no line inside one is an item. A fence that is
// never closed opens nothing: its line is ordinary text, so the items after it
// still count and an unfinished criterion cannot hide behind it.
func AcceptanceItems(text string) []AcceptanceItem {
	var items []AcceptanceItem
	for _, at := range acceptanceItemsAt(text) {
		items = append(items, at.AcceptanceItem)
	}
	return items
}

// acceptanceItemAt is an item with where it sits in its text: line is the
// index of its line in strings.Split(text, "\n"), boxAt is the byte offset of
// the "[" that opens its checkbox in that line, and box is the text after the
// checkbox, trimmed. boxAt and box are unset when the item is not boxed.
type acceptanceItemAt struct {
	AcceptanceItem
	line  int
	boxAt int
	box   string
}

// acceptanceItemsAt is AcceptanceItems with each item's line index, so a
// caller that rewrites one line finds it without a second parse.
func acceptanceItemsAt(text string) []acceptanceItemAt {
	var items []acceptanceItemAt
	lines := strings.Split(text, "\n")
	fence := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if closesFence(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if marker := FenceMarker(trimmed); marker != "" && fenceCloses(lines[i+1:], marker) {
			fence = marker
			continue
		}
		m := acceptanceItemRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		at := acceptanceItemAt{
			AcceptanceItem: AcceptanceItem{Line: trimmed, Nested: strings.TrimLeft(line, " \t") != line},
			line:           i,
		}
		// The box opens at the line's first "[": no list marker holds one, and
		// the box match is m[1], which lies inside line and starts with that "[".
		if box := acceptanceBoxRe.FindStringSubmatch(m[1]); box != nil {
			at.Boxed = true
			at.Checked = box[1] != " "
			at.boxAt = strings.Index(line, "[")
			at.box = strings.TrimSpace(m[1][len(box[0]):])
		}
		items = append(items, at)
	}
	return items
}

// ErrAcceptanceBoxNotFound is returned by TickAcceptanceBox when no unchecked
// box has the text asked for.
var ErrAcceptanceBoxNotFound = errors.New("no unchecked acceptance box has that text")

// ErrAcceptanceBoxAmbiguous is returned by TickAcceptanceBox when more than one
// unchecked box has the text asked for, so no one of them is the box meant.
var ErrAcceptanceBoxAmbiguous = errors.New("more than one unchecked acceptance box has that text")

// TickAcceptanceBox returns block with the one unchecked box whose text is box
// ticked. box is the text after the checkbox; a whole item line as bd prints
// it ("- [ ] text") names the same box. Only the "[ ]" of that line changes:
// every other byte of block is kept, so an amendment a writer made after the
// caller read block survives the tick. A box that is already ticked returns
// block unchanged. An unknown or ambiguous box is an error, never a guess.
func TickAcceptanceBox(block, box string) (string, error) {
	box = strings.TrimSpace(box)
	if m := acceptanceItemRe.FindStringSubmatch(box); m != nil {
		if b := acceptanceBoxRe.FindStringSubmatch(m[1]); b != nil {
			box = strings.TrimSpace(m[1][len(b[0]):])
		}
	}
	if box == "" {
		return "", errors.New("the acceptance box text is empty")
	}
	var open, ticked []acceptanceItemAt
	for _, at := range acceptanceItemsAt(block) {
		if !at.Boxed || at.box != box {
			continue
		}
		if at.Checked {
			ticked = append(ticked, at)
		} else {
			open = append(open, at)
		}
	}
	switch {
	case len(open) > 1:
		return "", ErrAcceptanceBoxAmbiguous
	case len(open) == 0 && len(ticked) > 0:
		return block, nil
	case len(open) == 0:
		return "", ErrAcceptanceBoxNotFound
	}
	lines := strings.Split(block, "\n")
	raw, at := lines[open[0].line], open[0].boxAt
	lines[open[0].line] = raw[:at+1] + "x" + raw[at+2:]
	return strings.Join(lines, "\n"), nil
}

// closesFence reports whether a trimmed line closes the fence that opened with
// marker: the same character, at least as long.
func closesFence(trimmed, marker string) bool {
	closer := FenceMarker(trimmed)
	return closer != "" && strings.HasPrefix(closer, marker)
}

// fenceCloses reports whether any of rest closes a fence opened with marker.
func fenceCloses(rest []string, marker string) bool {
	for _, line := range rest {
		if closesFence(strings.TrimSpace(line), marker) {
			return true
		}
	}
	return false
}

// FenceMarker reports the code fence a line opens: three or more backticks or
// tildes, optionally followed by an info string ("" when the line is not a
// fence). It is the opening and the closing form alike, so one helper serves
// both. A backtick fence's info string must not itself hold a backtick.
func FenceMarker(trimmed string) string {
	for _, b := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == b {
			n++
		}
		if n < 3 {
			continue
		}
		if b == '`' && strings.IndexByte(trimmed[n:], '`') >= 0 {
			continue
		}
		return trimmed[:n]
	}
	return ""
}
