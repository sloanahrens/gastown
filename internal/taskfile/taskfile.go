// Package taskfile assembles the TASK.md text a sandboxed polecat reads at
// spawn, so a seat can hold its whole assignment without reading a bead from
// Dolt.
//
// Assemble is pure: it takes the fields a caller already read out of the bead
// and the formula, and returns text. It never reads a bead, the clock or the
// environment, so the same Input always assembles to the same bytes and a
// golden test can pin the shape.
package taskfile

import (
	"strings"
	"time"
)

// commentTimeLayout is how a comment's time is written: the bead CLI's own
// "2026-11-14 22:40" shape, at minute precision, which is what a polecat reads
// in a bead comment today.
const commentTimeLayout = "2006-01-02 15:04"

// Box is one acceptance checkbox: the criterion's text and whether the bead has
// it ticked.
type Box struct {
	Text   string
	Ticked bool
}

// Note is one bead note. Notes carry no author or time of their own in a bead.
type Note struct {
	Text string
}

// Comment is one bead comment. Time is the zero time when the caller has no
// timestamp, and is then left out of the line rather than written as the Unix
// epoch.
type Comment struct {
	Author string
	Time   time.Time
	Text   string
}

// Step is one formula step, rendered as a checklist item. Text is the step's
// title, which the caller numbers if it wants the number shown; Done says
// whether the molecule has closed the step.
type Step struct {
	Text string
	Done bool
}

// Input is every field Assemble reads. A field left at its zero value is a
// field the caller did not pass, and it is left out of the text: no heading and
// no section for it.
type Input struct {
	BeadID      string
	Title       string
	Description string
	Boxes       []Box
	Notes       []Note
	Comments    []Comment
	Steps       []Step
}

// Assemble renders the TASK.md text for one seat. Sections appear in the order a
// reader wants them: what the bead is, what done means, what has been tried,
// what others said, and the steps to run. A section whose field is empty is
// left out whole, heading included, so the text never carries a heading over
// nothing.
//
// The text ends with exactly one newline. Slices render in caller order and
// nothing is sorted or looked up in a map, so the result is deterministic.
func Assemble(in Input) string {
	var chunks []string
	if head := heading(in); head != "" {
		chunks = append(chunks, head)
	}
	if body := strings.TrimRight(in.Description, "\n"); body != "" {
		chunks = append(chunks, "## Description\n\n"+body)
	}
	if section := assembleBoxes(in.Boxes); section != "" {
		chunks = append(chunks, section)
	}
	if section := assembleNotes(in.Notes); section != "" {
		chunks = append(chunks, section)
	}
	if section := assembleComments(in.Comments); section != "" {
		chunks = append(chunks, section)
	}
	if section := assembleSteps(in.Steps); section != "" {
		chunks = append(chunks, section)
	}
	if len(chunks) == 0 {
		return ""
	}
	return strings.Join(chunks, "\n\n") + "\n"
}

// heading is the one line naming the bead. Either the id or the title alone is
// enough to name it; only when the caller passed neither is there no heading.
func heading(in Input) string {
	id := strings.TrimSpace(in.BeadID)
	title := strings.TrimSpace(in.Title)
	switch {
	case id != "" && title != "":
		return "# TASK " + id + ": " + title
	case id != "":
		return "# TASK " + id
	case title != "":
		return "# TASK: " + title
	}
	return ""
}

func assembleBoxes(boxes []Box) string {
	var sb strings.Builder
	for _, box := range boxes {
		writeItem(&sb, checkbox(box.Ticked), box.Text)
	}
	return section("Acceptance", sb.String())
}

func assembleNotes(notes []Note) string {
	var sb strings.Builder
	for _, note := range notes {
		writeItem(&sb, "- ", note.Text)
	}
	return section("Notes", sb.String())
}

func assembleComments(comments []Comment) string {
	var sb strings.Builder
	for _, comment := range comments {
		writeItem(&sb, "- ", commentItem(comment))
	}
	return section("Comments", sb.String())
}

func assembleSteps(steps []Step) string {
	var sb strings.Builder
	for _, step := range steps {
		writeItem(&sb, checkbox(step.Done), step.Text)
	}
	return section("Steps", sb.String())
}

// section wraps items under a heading, or returns "" when there are none: an
// empty list must not leave a heading standing over nothing.
func section(name, items string) string {
	items = strings.TrimRight(items, "\n")
	if strings.TrimSpace(items) == "" {
		return ""
	}
	return "## " + name + "\n\n" + items
}

func checkbox(ticked bool) string {
	if ticked {
		return "- [x] "
	}
	return "- [ ] "
}

// commentItem is a comment's line after the bullet: its time and author when
// the caller passed them, then the text.
func commentItem(c Comment) string {
	var parts []string
	if !c.Time.IsZero() {
		// UTC, not the caller's zone: the same instant must assemble to the
		// same bytes on any host.
		parts = append(parts, c.Time.UTC().Format(commentTimeLayout))
	}
	if author := strings.TrimSpace(c.Author); author != "" {
		parts = append(parts, author)
	}
	text := strings.TrimRight(c.Text, "\n")
	if label := strings.Join(parts, " "); label != "" {
		if text == "" {
			return label
		}
		return label + ": " + text
	}
	return text
}

// writeItem writes one list item: the bullet prefix, the first line, and any
// further lines indented so the item stays one item in rendered markdown. A
// blank line inside the text stays empty rather than indented, so the file
// carries no trailing whitespace.
func writeItem(sb *strings.Builder, prefix, text string) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	sb.WriteString(strings.TrimRight(prefix+lines[0], " "))
	sb.WriteString("\n")
	for _, line := range lines[1:] {
		if line == "" {
			sb.WriteString("\n")
			continue
		}
		sb.WriteString("  ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
}
