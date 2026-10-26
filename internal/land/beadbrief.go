package land

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The openers a decision comment may use. Each is matched case-sensitively at
// the start of the comment's trimmed text. An opener grants nothing by itself:
// the author recorded on the comment is what entitles it (gt-0sztd).
const (
	sloanDecisionOpener = "SLOAN DECISION"
	overseerOpener      = "OVERSEER"
	rebuttalOpener      = "REBUTTAL:"
)

// beadBriefCap bounds BeadBrief in bytes, the room the work-item file gets
// before om truncates it.
const beadBriefCap = 16 << 10

// beadBriefDecisionFloor is the room BeadBrief keeps for the newest decision
// comment when the description's sections would fill the cap: the decision the
// next round must weigh outranks prose the reviewer can read in the description.
const beadBriefDecisionFloor = 4 << 10

const (
	beadBriefTitle            = "# Work item\n"
	beadBriefDecisionsHeading = "\n## Decision comments\n\n"
)

// beadBriefSections are the description sections the brief carries, in the
// order it carries them rather than the order the author wrote them.
var beadBriefSections = []string{"Goal", "Constraints", "Out of scope", "Gate"}

// decisionMarker is one opener a decision comment may use, with the author
// entitled to write it.
type decisionMarker struct {
	opener   string
	entitled func(author string) bool
}

// decisionMarkers is every comment the brief carries. Entitlement comes from
// the comment's Author field and nowhere else: an opener selects a comment only
// when that field is an identity entitled to the opener, since any commenter
// can type an opener (gt-0sztd). ACCEPTED-RISK: is acceptedRiskMarker, the
// opener criteria.go defines, read here with the same allowlist the
// accepted-risk context file uses.
var decisionMarkers = []decisionMarker{
	{sloanDecisionOpener, isSloanAuthor},
	{acceptedRiskMarker, isAcceptedRiskAuthor},
	{overseerOpener, isOverseerAuthor},
	{rebuttalOpener, anyAuthor},
}

// BeadBrief renders the work-item file om reads at landing (gt-0sztd): the
// bead's Goal, Constraints, Out of scope and Gate sections, then its decision
// comments oldest first, each under a line naming the author who wrote it.
//
// A section the description lacks or leaves empty is omitted, and a "## " line
// inside a section body is demoted to "### " so the body cannot open a section
// the description never had. The text is capped at beadBriefCap bytes by
// dropping the oldest comment first; the newest is kept even when the sections
// must be cut to make room for it, and cut to fit when it alone outgrows the
// cap. The cut lands on a rune boundary, so the file is valid UTF-8. A
// description with no sections and a bead with no qualifying comment leave the
// title alone.
//
// An attribution line is built only from the comment's Author and date fields.
// A comment's text is quoted line by line under it (quoteBody), so no line a
// commenter wrote can open a line that reads as an attribution of its own.
func BeadBrief(description string, comments []beads.Comment) string {
	head := beadBriefHead(description)
	entries := beadBriefDecisionEntries(comments)
	if len(entries) == 0 {
		return cutToBytes(head, beadBriefCap)
	}
	if room := beadBriefCap - beadBriefDecisionFloor; len(head) > room {
		head = cutToBytes(head, room)
	}
	// One byte stays for the trailing newline, so the file ends on a line.
	budget := beadBriefCap - len(head) - len(beadBriefDecisionsHeading) - 1
	if budget < 0 {
		budget = 0
	}
	return head + beadBriefDecisionsHeading + capEntries(entries, budget) + "\n"
}

// WriteBeadBrief writes BeadBrief's text to a 0600 temp file named
// om-brief-*.txt in dir and returns its path; the caller removes it after the
// review (gt-0sztd). The lander passes it to om as --bead-file (gt-3l5qy),
// which is why nothing in this bead calls it: the wiring is gt-3l5qy's, and
// deleting the writer would leave that bead to rebuild it.
func WriteBeadBrief(dir, description string, comments []beads.Comment) (string, error) {
	return writeTemp(dir, "om-brief-*.txt", []byte(BeadBrief(description, comments)))
}

// beadBriefHead renders the title and the sections the description carries, in
// beadBriefSections order, with the canonical heading spelling: the file reads
// the same whichever casing or order the author wrote.
func beadBriefHead(description string) string {
	sections := specdispatch.Sections(description)
	var b strings.Builder
	b.WriteString(beadBriefTitle)
	for _, name := range beadBriefSections {
		body := sections[strings.ToLower(name)]
		if body == "" {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", name, demoteSectionHeadings(body))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// demoteSectionHeadings moves any "## " line in a section body down to "### ",
// so the body cannot open a section of its own. A body reaches here with such a
// line as example text, from a fenced code block specdispatch.Sections keeps
// whole; in the brief it stays text too (gt-0sztd).
func demoteSectionHeadings(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		code := strings.TrimLeft(line, " \t")
		if !opensSectionHeading(code) {
			continue
		}
		lines[i] = line[:len(line)-len(code)] + "#" + code
	}
	return strings.Join(lines, "\n")
}

// opensSectionHeading reports whether line opens with a second-level heading
// marker: two hashes, then a space or a tab.
func opensSectionHeading(line string) bool {
	rest, ok := strings.CutPrefix(line, "##")
	return ok && (rest == "" || strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t"))
}

// beadBriefDecisionEntries returns the decision comments oldest first, each
// rendered by beadBriefDecisionEntry. When any date does not parse the list's
// own order stands, since no date can say where that comment belongs — the rule
// acceptedRiskComments uses.
func beadBriefDecisionEntries(comments []beads.Comment) []string {
	type decision struct {
		text string
		at   int64
	}
	kept := make([]decision, 0, len(comments))
	dated := true
	for _, c := range comments {
		text := strings.TrimSpace(c.Text)
		marker, ok := decisionMarkerFor(text, c.Author)
		if !ok {
			continue
		}
		at, parsed := commentTime(c.CreatedAt)
		dated = dated && parsed
		kept = append(kept, decision{text: beadBriefDecisionEntry(marker, c), at: at})
	}
	if dated {
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].at < kept[j].at })
	}
	entries := make([]string, len(kept))
	for i, d := range kept {
		entries[i] = d.text
	}
	return entries
}

// decisionMarkerFor returns the opener text starts with when its author is
// entitled to that opener.
func decisionMarkerFor(text, author string) (string, bool) {
	for _, m := range decisionMarkers {
		if strings.HasPrefix(text, m.opener) && m.entitled(author) {
			return m.opener, true
		}
	}
	return "", false
}

// beadBriefDecisionEntry renders one comment under the line that attributes it:
// om weighs a named author's decision, and an assertion with no one behind it is
// not a decision to weigh. The line is built from the Author and date fields
// alone, and the text is quoted beneath it, so a body that embeds an
// attribution header of its own stays body (gt-0sztd).
func beadBriefDecisionEntry(marker string, c beads.Comment) string {
	date := headerField(c.CreatedAt)
	if date == "" {
		date = "an unrecorded date"
	}
	return fmt.Sprintf("[%s from %s, %s]\n%s", marker, headerField(c.Author), date, quoteBody(strings.TrimSpace(c.Text)))
}

// quoteBody puts every line of a comment's text under a ">" marker, so a line
// that would read as an attribution header is quoted instead: the header om
// weighs is the one the code wrote from Author and date, never text the
// commenter supplied (gt-0sztd).
func quoteBody(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			lines[i] = ">"
			continue
		}
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

// isSloanAuthor reports whether author is the human who records SLOAN DECISION
// comments.
func isSloanAuthor(author string) bool {
	return strings.TrimSpace(author) == "Sloan Ahrens"
}

// isOverseerAuthor reports whether author is the town's overseer, the identity
// that writes OVERSEER comments.
func isOverseerAuthor(author string) bool {
	return strings.TrimSpace(author) == "town/crew/overseer"
}

// anyAuthor entitles every author. A rebuttal is the work's own answer and
// claims no authority a reviewer must accept: the brief carries it with its
// author named so om checks the claim against the diff, which is why it is not
// gated like the three decision openers.
func anyAuthor(string) bool { return true }
