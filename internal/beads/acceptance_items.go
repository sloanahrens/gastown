package beads

import (
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
// block is example text, so no line inside one is an item.
func AcceptanceItems(text string) []AcceptanceItem {
	var items []AcceptanceItem
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if marker := FenceMarker(trimmed); marker != "" && strings.HasPrefix(marker, fence) {
				fence = ""
			}
			continue
		}
		if fence = FenceMarker(trimmed); fence != "" {
			continue
		}
		m := acceptanceItemRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		item := AcceptanceItem{Line: trimmed, Nested: strings.TrimLeft(line, " \t") != line}
		if box := acceptanceBoxRe.FindStringSubmatch(m[1]); box != nil {
			item.Boxed = true
			item.Checked = box[1] != " "
		}
		items = append(items, item)
	}
	return items
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
