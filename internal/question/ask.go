package question

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Hold is one piece of work a question holds up: a bead that exists (ID), or
// work with no bead yet (Stub, the title of the bead to create first).
type Hold struct {
	ID   string
	Stub string
}

// Ask is a question as gt ask reads it from its arguments and stdin.
type Ask struct {
	Title string
	// Body is the description filed on the bead: the text gt ask was given.
	Body  string
	Holds []Hold
	// Priority is the priority the question bead is filed at, bd's 0-4.
	Priority int
	// Effort is the tag for what answering takes, "" for none.
	Effort string
}

// AskOptions are gt ask's arguments that are not the body: the flags it was
// given. Priority's zero value is P0, a real priority, so a caller with a
// default passes it rather than leaving the field out.
type AskOptions struct {
	Priority int
	Effort   string
}

// Labels are the labels the question bead is filed with: the overseer-question
// label every question carries, and the effort tag when there is one.
func (a Ask) Labels() []string {
	labels := []string{Label}
	if l := EffortLabel(a.Effort); l != "" {
		labels = append(labels, l)
	}
	return labels
}

// stubPrefix leads a ## Held work line that names work with no bead yet.
const stubPrefix = "stub:"

// ParseAsk checks the title, the flags and the body gt ask was given and reads
// the work the question holds. A body without a ## Question or a ## Recommended
// answer section is refused: a question Sloan cannot answer from one glance is
// not worth waking for. ## Held work and ## Facts are optional, and a missing
// ## Held work holds nothing.
//
// A held line that is neither a bead id, "stub: <title>" nor "none" is an
// error, so a malformed line files nothing rather than half a question.
func ParseAsk(title, body string, opts AskOptions) (Ask, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Ask{}, fmt.Errorf("a question needs a title")
	}
	if n := utf8.RuneCountInString(title); n > TitleMax {
		return Ask{}, fmt.Errorf("title is %d characters, over the %d limit", n, TitleMax)
	}
	if err := CheckPriority(opts.Priority); err != nil {
		return Ask{}, err
	}
	if err := CheckEffort(opts.Effort); err != nil {
		return Ask{}, err
	}
	sections := Sections(body)
	for _, s := range []struct{ key, name string }{
		{HeadingQuestion, "## Question"},
		{HeadingRecommended, "## Recommended answer"},
	} {
		if strings.TrimSpace(sections[s.key]) == "" {
			return Ask{}, fmt.Errorf("section %q is missing or empty", s.name)
		}
	}
	holds, err := parseHolds(sections[HeadingHeld])
	if err != nil {
		return Ask{}, err
	}
	return Ask{Title: title, Body: strings.TrimSpace(body), Holds: holds,
		Priority: opts.Priority, Effort: opts.Effort}, nil
}

// PriorityMin and PriorityMax bound the priority a question is filed at, bd's
// own range: 0 is the most urgent.
const (
	PriorityMin = 0
	PriorityMax = 4
)

// PushMaxPriority is the lowest priority a question is pushed to the human at.
// A question at P0, P1 or P2 is one the human must answer before the work it
// holds can move; a P3 or P4 waits on the dashboard instead of in a text
// (gt-71714).
const PushMaxPriority = 2

// PushPriority reports whether a question at priority is worth a push.
func PushPriority(priority int) bool {
	return priority >= PriorityMin && priority <= PushMaxPriority
}

// PushTitle is the one line a push carries: the question's id, so the text
// names the dashboard row it is about, its priority and its title. The body is
// never pushed, and the title is drawn the way the dashboard draws it — Field
// flattens it to one line, drops the control and invisible format characters,
// and caps its length — so what a lock screen shows is what the row says
// (gt-71714).
func PushTitle(id string, priority int, title string) string {
	return fmt.Sprintf("Question %s P%d: %s", id, priority, Field(title))
}

// CheckPriority refuses a priority outside bd's range.
func CheckPriority(priority int) error {
	if priority < PriorityMin || priority > PriorityMax {
		return fmt.Errorf("--priority %d is outside bd's %d-%d range", priority, PriorityMin, PriorityMax)
	}
	return nil
}

// CheckEffort refuses an effort tag gt ask does not write.
func CheckEffort(effort string) error {
	switch effort {
	case "", EffortQuick, EffortSession:
		return nil
	}
	return fmt.Errorf("--effort %q is not %s or %s", effort, EffortQuick, EffortSession)
}

// parseHolds reads a ## Held work section: one bead id, "stub: <title>" or
// "none" per line, each with an optional "- " bullet. An id may carry an
// annotation after it.
func parseHolds(section string) ([]Hold, error) {
	var out []Hold
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		switch {
		case line == "" || strings.EqualFold(line, "none"):
		case strings.HasPrefix(line, stubPrefix):
			stub := strings.TrimSpace(strings.TrimPrefix(line, stubPrefix))
			if stub == "" {
				return nil, fmt.Errorf("held work has an empty stub title")
			}
			out = append(out, Hold{Stub: stub})
		default:
			id := strings.Fields(line)[0]
			if !IsBeadID(id) {
				return nil, fmt.Errorf("held work %q is not a bead id (use an id, 'stub: <title>' or 'none')", id)
			}
			out = append(out, Hold{ID: id})
		}
	}
	return out, nil
}

// ReplaceStub points the first ## Held work line that names the stub at the id
// of the bead made for it, so a reader lists the bead the question holds. It
// returns body unchanged when no line names the stub.
func ReplaceStub(body, stub, id string) string {
	lines := strings.Split(body, "\n")
	inHeld := false
	for i, line := range lines {
		if name, ok := headingName(line); ok {
			inHeld = name == HeadingHeld
			continue
		}
		if !inHeld {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		title, ok := strings.CutPrefix(rest, stubPrefix)
		if ok && strings.TrimSpace(title) == stub {
			lines[i] = "- " + id
			break
		}
	}
	return strings.Join(lines, "\n")
}
