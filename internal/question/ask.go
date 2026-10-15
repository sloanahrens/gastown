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
}

// stubPrefix leads a ## Held work line that names work with no bead yet.
const stubPrefix = "stub:"

// ParseAsk checks the title and body gt ask was given and reads the work the
// question holds. A body without a ## Question or a ## Recommended answer
// section is refused: a question Sloan cannot answer from one glance is not
// worth waking for. ## Held work and ## Facts are optional, and a missing
// ## Held work holds nothing.
//
// A held line that is neither a bead id, "stub: <title>" nor "none" is an
// error, so a malformed line files nothing rather than half a question.
func ParseAsk(title, body string) (Ask, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Ask{}, fmt.Errorf("a question needs a title")
	}
	if n := utf8.RuneCountInString(title); n > TitleMax {
		return Ask{}, fmt.Errorf("title is %d characters, over the %d limit", n, TitleMax)
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
	return Ask{Title: title, Body: strings.TrimSpace(body), Holds: holds}, nil
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
