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
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// commentTimeLayout is how a comment's time is written: the bead CLI's own
// "2026-11-14 22:40" shape, at minute precision, which is what a polecat reads
// in a bead comment today.
const commentTimeLayout = "2006-01-02 15:04"

// maxNotes caps the notes section at the newest notes: a long bead's oldest
// notes matter least, and the seat reads the file at spawn.
const maxNotes = 20

// maxTextKiB and maxTextBytes cap the whole assembled text at 64 KiB, so one
// TASK.md stays small enough to hand a seat at spawn.
const (
	maxTextKiB   = 64
	maxTextBytes = maxTextKiB * 1024
)

// truncationMarker is the line appended to text that exceeded maxTextBytes. It
// states the cap so a reader knows the task text they hold is incomplete. The
// size is read from maxTextKiB rather than written a second time, so the marker
// cannot name a cap the assembler no longer applies.
func truncationMarker() string {
	return fmt.Sprintf("[TASK.md truncated: the text exceeded %d KiB and the rest is not shown]", maxTextKiB)
}

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

// Finding is one review finding an earlier landing refusal recorded: the id
// the rework pass tracks, its severity, where it points and its title. The
// rejection note keeps no finding detail, so none is carried.
type Finding struct {
	ID       string
	Severity string
	Path     string
	Line     int
	Title    string
}

// Rejection is one MERGE REJECTION block: an earlier landing attempt of the
// bead and the findings it was refused for.
type Rejection struct {
	Attempt  int
	Kind     string
	Reason   string
	Findings []Finding
}

// Rework is a re-slung seat's rework context. A first-time seat carries none,
// and the text then reads as it did before rework existed.
type Rework struct {
	// Attempts are the bead's earlier refusals, in the order Land recorded
	// them, each with the findings it filed.
	Attempts []Rejection
	// Unchanged is the latest refusal naming the commit the seat's branch
	// still sits on, nil when the branch moved past every refusal. The host
	// computes it once at spawn: a seat with no bead access cannot ask again.
	Unchanged *UnchangedRejection
}

// UnchangedRejection is the refusal whose commit the seat's branch still sits
// on. Head is shown abbreviated.
type UnchangedRejection struct {
	Attempt int
	Kind    string
	Head    string
}

// Input is every field Assemble reads. A field left at its zero value is a
// field the caller did not pass, and it is left out of the text: no heading and
// no section for it. A list entry with no text is left out the same way, so a
// zero-value entry neither renders a bullet nor keeps its heading alive.
type Input struct {
	BeadID      string
	Title       string
	Description string
	Boxes       []Box
	Notes       []Note
	Comments    []Comment
	Steps       []Step
	Rework      *Rework
}

// Assemble renders the TASK.md text for one seat. Sections appear in the order a
// reader wants them: what the bead is, what done means, what has been tried,
// what others said, what earlier attempts were refused for, and the steps to
// run. A section whose field is empty is left out whole, heading included, so
// the text never carries a heading over nothing.
//
// The text ends with exactly one newline. Slices render in caller order and
// nothing is sorted or looked up in a map, so the result is deterministic.
//
// Two caps keep the file small: the notes section keeps only the newest
// maxNotes entries, and a text longer than maxTextBytes is cut and ends with
// truncationMarker on its own last line.
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
	if section := assembleRejections(in.Rework); section != "" {
		chunks = append(chunks, section)
	}
	if section := assembleRejectionUnchanged(in.Rework); section != "" {
		chunks = append(chunks, section)
	}
	if section := assembleSteps(in.Steps); section != "" {
		chunks = append(chunks, section)
	}
	if len(chunks) == 0 {
		return ""
	}
	return capText(strings.Join(chunks, "\n\n") + "\n")
}

// capText cuts text that exceeds maxTextBytes to fit it, marker included, and
// ends the result with truncationMarker on its last line. It cuts at a rune
// boundary, so the result is valid UTF-8 whatever the caller wrote; a trailing
// partial line is kept, and the marker sits on the line after it.
func capText(text string) string {
	if len(text) <= maxTextBytes {
		return text
	}
	// The marker, the newline that starts its line and the final newline are
	// part of the cap, so the result never exceeds maxTextBytes.
	marker := truncationMarker()
	budget := maxTextBytes - len(marker) - 2
	cut := budget
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimRight(text[:cut], "\n") + "\n" + marker + "\n"
}

// heading is the one line naming the bead. Either the id or the title alone is
// enough to name it; only when the caller passed neither is there no heading.
func heading(in Input) string {
	id := oneLine(in.BeadID)
	title := oneLine(in.Title)
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

// oneLine folds the whitespace in s to single spaces. A newline written raw in
// the id or the title would end the heading line and turn the rest into body
// text, so the heading is built from oneLine's result.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func assembleBoxes(boxes []Box) string {
	var sb strings.Builder
	for _, box := range boxes {
		writeItem(&sb, checkbox(box.Ticked), box.Text)
	}
	return section("Acceptance", sb.String())
}

// assembleNotes renders the newest notes only. The cap counts the caller's
// entries before empty ones drop out, so a trailing empty entry can leave fewer
// than maxNotes bullets, never more.
func assembleNotes(notes []Note) string {
	if len(notes) > maxNotes {
		notes = notes[len(notes)-maxNotes:]
	}
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

// assembleRejections renders every earlier refusal with the findings it filed.
// Each finding says what the next attempt has to answer: its id, severity, the
// place it points at and its title. The attempt header names the refusal's
// class and reason, which a block with neither leaves as the bare number.
func assembleRejections(rw *Rework) string {
	if rw == nil {
		return ""
	}
	var blocks []string
	for _, r := range rw.Attempts {
		var sb strings.Builder
		header := fmt.Sprintf("### Attempt %d", r.Attempt)
		var parts []string
		if kind := oneLine(r.Kind); kind != "" {
			parts = append(parts, kind)
		}
		if reason := oneLine(r.Reason); reason != "" {
			parts = append(parts, reason)
		}
		if len(parts) > 0 {
			header += ": " + strings.Join(parts, " — ")
		}
		sb.WriteString(header + "\n")
		for _, f := range r.Findings {
			writeItem(&sb, "- ", findingItem(f))
		}
		blocks = append(blocks, strings.TrimRight(sb.String(), "\n"))
	}
	return section("Earlier rejections", strings.Join(blocks, "\n\n"))
}

// findingItem is one finding's line after the bullet, in the shape the bead's
// rejection note writes it: the fields it records, then the title.
func findingItem(f Finding) string {
	var head []string
	if id := oneLine(f.ID); id != "" {
		head = append(head, "id:"+id)
	}
	if severity := oneLine(f.Severity); severity != "" {
		head = append(head, "sev:"+severity)
	}
	if path := oneLine(f.Path); path != "" {
		head = append(head, fmt.Sprintf("%s:%d", path, f.Line))
	}
	label := strings.Join(head, " ")
	title := strings.TrimRight(f.Title, "\n")
	switch {
	case label == "":
		return title
	case title == "":
		return label
	}
	return label + " — " + title
}

// assembleRejectionUnchanged states that the branch the seat starts on holds
// the content a refusal already judged, and what that means for submitting it
// again. The seat cannot recompute this: it has no bead access.
func assembleRejectionUnchanged(rw *Rework) string {
	if rw == nil || rw.Unchanged == nil {
		return ""
	}
	u := rw.Unchanged
	refusal := fmt.Sprintf("attempt %d", u.Attempt)
	if kind := oneLine(u.Kind); kind != "" {
		refusal += " (" + kind + ")"
	}
	var b strings.Builder
	if head := shortHead(u.Head); head != "" {
		fmt.Fprintf(&b, "Your branch still sits on %s, the commit that %s refused; nothing has been committed since.\n\n", head, refusal)
	} else {
		fmt.Fprintf(&b, "Your branch still sits on the commit that %s refused; nothing has been committed since.\n\n", refusal)
	}
	b.WriteString("If that refusal came from this diff, a resubmission of the same content is refused as no change: change the content so it answers the findings, or escalate if the refusal did not come from this diff.")
	return section("Rejection unchanged", b.String())
}

// shortHead abbreviates a commit to git's default seven characters, the id a
// polecat knows it by. A value already shorter is shown as written.
func shortHead(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
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
// carries no trailing whitespace. An item with no text writes nothing, so a
// zero-value entry leaves no bare bullet behind.
func writeItem(sb *strings.Builder, prefix, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
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
