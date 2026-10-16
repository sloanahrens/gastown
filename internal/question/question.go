// Package question is the overseer's question convention: a bead in the town
// database under the overseer-question label, whose description carries
// "## Question", "## Recommended answer", "## Held work" and "## Facts"
// sections and whose comments carry the conversation. The overseer asks with
// gt ask, Sloan answers with gt answer, and three readers share the one
// parse: gt questions, the dashboard's Needs you panel and the daemon's
// question collector.
package question

import (
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
)

const (
	// Label is the label a question is filed under.
	Label = "overseer-question"
	// Statuses are the statuses a question can still be answered in.
	Statuses = "open,in_progress,blocked"

	// OverseerPrefix marks a comment as the overseer's own. A question is
	// waiting while the newest comment carries it, and answered once Sloan has
	// replied without it: bead authors are the git committer, the same for
	// both, so the prefix is the only way to tell them apart.
	OverseerPrefix = "[overseer]"

	// FieldMax caps a parsed field in runes. The description is the
	// overseer's own text and a reader has to draw it whatever it holds; the
	// cap bounds a pane a runaway description would otherwise fill.
	FieldMax = 1000

	// TitleMax is the longest title gt ask files.
	TitleMax = 120

	// Priority is the priority gt ask files a question at when it is given
	// none: bd's middle, so a question that says nothing else queues with
	// ordinary work.
	Priority = 2

	// EffortQuick and EffortSession are the effort tags gt ask writes: a
	// question answerable from one glance, and one that wants a sitting.
	EffortQuick   = "quick"
	EffortSession = "session"

	// EffortPrefix leads the label an effort tag is written as, so the tag
	// rides the bead's own labels and needs no new store.
	EffortPrefix = "effort:"
)

// The description's section headings, as the parse keys them (lowercased).
const (
	HeadingQuestion    = "question"
	HeadingRecommended = "recommended answer"
	HeadingHeld        = "held work"
	HeadingFacts       = "facts"
)

// ListOptions is the query every reader asks for: every question still open,
// whatever its priority and however many there are. Priority is the no-filter
// sentinel -1 rather than the zero value, which bd reads as a P0-only filter.
var ListOptions = beads.ListOptions{
	Label:    Label,
	Status:   Statuses,
	Priority: -1,
	Limit:    0,
}

// beadID matches a bead id: a lowercase prefix, a hyphen, then the id's runs of
// lowercase letters, digits, dots and hyphens. "gt-cr62m", "hq-cv-abc" and
// "gt-wisp-590yk" are ids; a word of the section's prose is not.
var beadID = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9][a-z0-9.-]*$`)

// IsBeadID reports whether s has the shape of a bead id.
func IsBeadID(s string) bool { return beadID.MatchString(s) }

// EffortLabel is the label an effort tag is written as, and "" for no tag.
func EffortLabel(effort string) string {
	if effort == "" {
		return ""
	}
	return EffortPrefix + effort
}

// EffortOf reads a question's effort tag off its labels, and "" when it carries
// none. The tag is returned as written: a reader that second-guessed a
// hand-added label would hide the operator's own note instead of showing it.
func EffortOf(labels []string) string {
	for _, l := range labels {
		if tag, ok := strings.CutPrefix(l, EffortPrefix); ok && tag != "" {
			return tag
		}
	}
	return ""
}

// Fields are the parts of a question bead a reader draws.
type Fields struct {
	Title string
	// Question and Recommended are the ## Question and ## Recommended answer
	// sections, each as one line, or empty when the description lacks them.
	Question    string
	Recommended string
	// Held are the bead ids the ## Held work section names.
	Held []string
	// Malformed marks a description without both sections a reader needs.
	Malformed bool
}

// Parse reads one listed bead: the title, the sections of its description, and
// whether that description is shaped the way a reader needs.
func Parse(issue *beads.Issue) Fields {
	sections := Sections(issue.Description)
	q := Field(sections[HeadingQuestion])
	r := Field(sections[HeadingRecommended])
	return Fields{
		Title:       Field(issue.Title),
		Question:    q,
		Recommended: r,
		Held:        HeldIDs(sections[HeadingHeld]),
		Malformed:   q == "" || r == "",
	}
}

// Answered reports whether a question's newest comment is Sloan's: the newest
// comment without the overseer prefix is his answer, and the overseer is to act
// on it. A question with no comment, or whose newest comment is the
// overseer's, is still Sloan's to answer.
func Answered(comments []beads.Comment) bool {
	newest, ok := NewestComment(comments)
	return ok && !strings.HasPrefix(newest.Text, OverseerPrefix)
}

// NewestComment is the comment a question's state is read from: the latest by
// its own time, and the last one listed when no time parses, which is the
// newest when bd lists them oldest first.
func NewestComment(comments []beads.Comment) (beads.Comment, bool) {
	if len(comments) == 0 {
		return beads.Comment{}, false
	}
	newest, newestAt := comments[len(comments)-1], time.Time{}
	for _, c := range comments {
		at, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil || at.Before(newestAt) {
			continue
		}
		newest, newestAt = c, at
	}
	return newest, true
}

// AskedAt is when a question bead was created, and the zero time for a bead
// whose creation time does not parse.
func AskedAt(issue *beads.Issue) time.Time {
	at, _ := time.Parse(time.RFC3339, issue.CreatedAt)
	return at
}

// AgeSec is how long ago a question was asked, nil for a bead that records no
// time it can be measured from: an age of zero would read as "asked just now".
func AgeSec(now, askedAt time.Time) *int64 {
	if askedAt.IsZero() {
		return nil
	}
	d := now.Sub(askedAt)
	if d < 0 {
		d = 0
	}
	sec := int64(d / time.Second)
	return &sec
}

// Sections reads a description's "## Heading" sections, keyed by the
// lowercased heading so a differently capitalised one still parses. A
// description the overseer's tool did not shape yields whatever headings it
// does carry.
func Sections(description string) map[string]string {
	out := map[string]string{}
	heading := ""
	var body []string
	flush := func() {
		if heading != "" {
			out[heading] = strings.Join(body, "\n")
		}
		body = nil
	}
	for _, line := range strings.Split(description, "\n") {
		if name, ok := headingName(line); ok {
			flush()
			heading = name
			continue
		}
		if heading != "" {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// headingName reads a "## Question" line as its heading name, lowercased. A
// line with more than two hashes is a deeper heading the description does not
// define, so it is body text.
func headingName(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "##")
	if !ok || strings.HasPrefix(rest, "#") {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(rest)), true
}

// Field is one parsed field as a reader draws it: one line, with every control
// and invisible format character gone — the dashboard writes these fields with
// textContent, and a rune that reorders what the operator reads is as much a
// hazard as a tag — and capped at FieldMax runes.
func Field(s string) string {
	flat := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	return CapRunes(strings.Join(strings.Fields(flat), " "), FieldMax)
}

// HeldIDs are the bead ids a ## Held work section names. gt ask writes one id
// per "- " line and may append an annotation after it, so a bullet line yields
// its first token only: the annotation is prose, and its hyphenated words
// ("follow-up", "re-run") match the id shape. A bullet whose first token is
// "none" or "stub:" names no bead. A line that is not a bullet is free prose
// from a hand-written description; its tokens are kept when shaped like an id.
// Either way an id a reader draws is one the operator can paste into bd.
func HeldIDs(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "- "); ok {
			if first := strings.Fields(rest); len(first) > 0 {
				if id := strings.Trim(first[0], "._-,;:"); IsBeadID(id) {
					out = append(out, id)
				}
			}
			continue
		}
		for _, token := range strings.FieldsFunc(line, func(r rune) bool {
			return !(unicode.IsLower(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_')
		}) {
			token = strings.Trim(token, "._-")
			if IsBeadID(token) {
				out = append(out, token)
			}
		}
	}
	return out
}

// CapRunes truncates s to n runes. A cap on bytes would cut a multi-byte rune
// in half and hand the page an invalid one.
func CapRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
