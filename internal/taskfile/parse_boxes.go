package taskfile

import (
	"fmt"
	"strings"
)

// acceptanceHeading is the line Assemble writes above a seat's acceptance boxes.
const acceptanceHeading = "## Acceptance"

// ParseBoxes returns the acceptance boxes in TASK.md text that Assemble rendered,
// in order, with their text and ticked state. It is pure: the caller passes the
// text it read.
//
// The section is the last line that is exactly "## Acceptance", since Assemble
// writes the description raw above it. It ends at the next "## " line, which
// Assemble writes only above the sections after it, so a text cut in one of those
// keeps every box. Text with no acceptance heading has no boxes and returns nil,
// unless the size cap cut it, since the heading may have been cut away. A heading
// with no boxes, a line in the section that is not a box or its continuation, and
// the truncation marker are errors, so a cut-short file fails closed. CRLF lines
// are read as LF lines.
//
// Limits, accepted and not fixed: with no boxes at all, a description line
// "## Acceptance" is read as the section, and a description that alone exceeds the
// cap and carries those headings is not detected. Assemble trims the spaces at the
// end of a box's first line, so the parsed text drops them.
func ParseBoxes(text string) ([]Box, error) {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	start := -1
	for i, line := range lines {
		if line == acceptanceHeading {
			start = i + 1
		}
	}
	if start < 0 {
		if endsWithCapMarker(lines) {
			return nil, fmt.Errorf("taskfile: text is cut by the size cap before its acceptance heading, so its boxes are unknown")
		}
		return nil, nil
	}

	var boxes []Box
	var cur *Box
	curLine := 0
	// blanks counts the blank lines since the last content line. Assemble writes
	// a blank line inside a box's text as an empty line, so the blanks belong to
	// the box only when a continuation line follows them; a blank line before the
	// next section belongs to nothing.
	blanks := 0
	closeBox := func() error {
		if cur == nil {
			return nil
		}
		if strings.TrimSpace(cur.Text) == "" {
			return fmt.Errorf("taskfile: acceptance box at line %d has no text", curLine)
		}
		boxes = append(boxes, *cur)
		cur = nil
		return nil
	}

	for i := start; i < len(lines); i++ {
		line := lines[i]
		lineNo := i + 1
		if strings.HasPrefix(line, "## ") {
			break
		}
		if line == "" {
			blanks++
			continue
		}
		if ticked, first, ok := boxStart(line); ok {
			if err := closeBox(); err != nil {
				return nil, err
			}
			cur, curLine, blanks = &Box{Text: first, Ticked: ticked}, lineNo, 0
			continue
		}
		if strings.HasPrefix(line, "  ") {
			if cur == nil {
				return nil, fmt.Errorf("taskfile: acceptance line %d continues a box that does not exist", lineNo)
			}
			// writeItem indents every continuation line by two spaces and writes
			// each blank line inside a box as an empty line.
			cur.Text += strings.Repeat("\n", blanks+1) + line[2:]
			blanks = 0
			continue
		}
		return nil, fmt.Errorf("taskfile: acceptance line %d is not a box: want \"- [ ] \" or \"- [x] \" at the start of the line", lineNo)
	}
	if err := closeBox(); err != nil {
		return nil, err
	}
	if len(boxes) == 0 {
		return nil, fmt.Errorf("taskfile: the acceptance section has no boxes")
	}
	return boxes, nil
}

// endsWithCapMarker reports whether the last non-empty line is the marker that
// capText writes when it cuts text to fit the size cap.
func endsWithCapMarker(lines []string) bool {
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return len(lines) > 0 && lines[len(lines)-1] == truncationMarker()
}

// boxStart reports whether line opens a box, and if so its ticked state and the
// first line of its text. A box whose text starts with a newline opens with a
// bare "- [ ]" or "- [x]", since writeItem trims the space after the bullet.
func boxStart(line string) (ticked bool, first string, ok bool) {
	switch {
	case line == "- [ ]":
		return false, "", true
	case line == "- [x]":
		return true, "", true
	case strings.HasPrefix(line, "- [ ] "):
		return false, line[len("- [ ] "):], true
	case strings.HasPrefix(line, "- [x] "):
		return true, line[len("- [x] "):], true
	}
	return false, "", false
}
