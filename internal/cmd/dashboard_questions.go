package cmd

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The Questions panel: the questions the overseer has filed for Sloan, so he
// can see what is waiting on him and answer it from a terminal. It reads the
// town's own beads database, which is where a question is filed — a reader
// that listed from a rig worktree would see that rig's beads alone.
//
// A read that fails is a named state rather than an empty list, because "the
// overseer has asked nothing" and "the answer could not be read" are different
// things to tell the operator.

const (
	// questionsLabel is the label the overseer files a question under.
	questionsLabel = "overseer-question"
	// questionsStatuses are the statuses a question can still be answered in.
	questionsStatuses = "open,in_progress,blocked"

	// dashQuestionRows is how many questions the pane draws. A longer queue is
	// counted in More rather than truncated silently, and the rows past the cap
	// cost no read of their own.
	dashQuestionRows = 20

	// questionFieldMax caps a parsed field in runes. The description is the
	// overseer's own text and the pane has to draw it whatever it holds; the cap
	// bounds a pane a runaway description would otherwise fill.
	questionFieldMax = 1000

	// questionsOverseerPrefix marks a comment as the overseer's own. A question
	// is waiting while the newest comment carries it, and answered once Sloan
	// has replied without it.
	questionsOverseerPrefix = "[overseer]"

	// The description's section headings: the overseer's question, the answer
	// it recommends, and the work the question holds up.
	questionHeadingQuestion    = "question"
	questionHeadingRecommended = "recommended answer"
	questionHeadingHeld        = "held work"
)

// questionBeadID matches a bead id: a lowercase prefix, a hyphen, then the id's
// runs of lowercase letters, digits, dots and hyphens. "gt-cr62m", "hq-cv-abc"
// and "gt-wisp-590yk" are ids; a word of the section's prose is not.
var questionBeadID = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9][a-z0-9.-]*$`)

// dashQuestionsReader reads the town's open overseer questions. list and
// comments are injectable so the row building is testable without a database.
type dashQuestionsReader struct {
	list     func() ([]*beads.Issue, error)
	comments func(id string) ([]beads.Comment, error)
}

// questionListOptions is the query the panel asks for: every question still
// open, whatever its priority and however many there are. Priority is the
// no-filter sentinel -1 rather than the zero value, which bd reads as a P0-only
// filter — a pane asking for P0 would show nothing for a question at any other
// priority.
var questionListOptions = beads.ListOptions{
	Label:    questionsLabel,
	Status:   questionsStatuses,
	Priority: -1,
	Limit:    0,
}

func newDashQuestionsReader(townRoot string) *dashQuestionsReader {
	b := beads.New(beads.ResolveBeadsDir(townRoot))
	return &dashQuestionsReader{
		list:     func() ([]*beads.Issue, error) { return b.List(questionListOptions) },
		comments: b.Comments,
	}
}

// read is the pane's whole reading. A read that fails at any point is an
// unavailable pane: the page cannot draw "awaiting you" for a question whose
// comments it could not read, since that is the one wrong answer it could give.
func (r *dashQuestionsReader) read(now time.Time) *dashboard.Questions {
	issues, err := r.list()
	if err != nil {
		return &dashboard.Questions{Unavailable: true}
	}
	candidates := make([]questionCandidate, 0, len(issues))
	for _, issue := range issues {
		candidates = append(candidates, questionRow(issue))
	}
	sortQuestionRows(candidates)

	out := &dashboard.Questions{Rows: make([]dashboard.QuestionRow, 0, len(candidates))}
	if len(candidates) > dashQuestionRows {
		out.More = len(candidates) - dashQuestionRows
		candidates = candidates[:dashQuestionRows]
	}
	for _, c := range candidates {
		comments, err := r.comments(c.row.ID)
		if err != nil {
			return &dashboard.Questions{Unavailable: true}
		}
		c.row.Comments = len(comments)
		c.row.State = questionState(comments)
		c.row.AgeSec = questionAgeSec(now, c.askedAt)
		out.Rows = append(out.Rows, c.row)
	}
	return out
}

// questionCandidate is one listed bead and the time the queue orders it by.
type questionCandidate struct {
	row     dashboard.QuestionRow
	askedAt time.Time // zero when the bead's creation time does not parse
}

// questionRow parses one listed bead: the title, the sections of its
// description, and whether that description is shaped the way the pane needs.
func questionRow(issue *beads.Issue) questionCandidate {
	sections := questionSections(issue.Description)
	question := questionField(sections[questionHeadingQuestion])
	recommended := questionField(sections[questionHeadingRecommended])
	askedAt, _ := time.Parse(time.RFC3339, issue.CreatedAt)
	return questionCandidate{
		row: dashboard.QuestionRow{
			ID:          issue.ID,
			Title:       questionField(issue.Title),
			Question:    question,
			Recommended: recommended,
			Held:        questionHeldIDs(sections[questionHeadingHeld]),
			Malformed:   question == "" || recommended == "",
		},
		askedAt: askedAt,
	}
}

// sortQuestionRows orders the queue oldest first, so the question that has
// waited longest is the first the operator reads, and puts a bead with no time
// it can be measured from after every one that has a time: an unreadable
// creation time must not sort as the longest wait.
func sortQuestionRows(candidates []questionCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].askedAt, candidates[j].askedAt
		switch {
		case a.IsZero() != b.IsZero():
			return b.IsZero()
		case a.IsZero():
			return false
		}
		return a.Before(b)
	})
}

// questionState reads a question's standing off its newest comment: the
// overseer's own comments carry a "[overseer]" prefix, so the newest comment
// without one is Sloan's answer.
func questionState(comments []beads.Comment) string {
	newest, ok := newestComment(comments)
	if !ok || strings.HasPrefix(newest.Text, questionsOverseerPrefix) {
		return dashboard.QuestionAwaiting
	}
	return dashboard.QuestionAnswered
}

// newestComment is the comment a question's state is read from: the latest by
// its own time, and the last one listed when no time parses, which is the
// newest when bd lists them oldest first.
func newestComment(comments []beads.Comment) (beads.Comment, bool) {
	if len(comments) == 0 {
		return beads.Comment{}, false
	}
	newest, newestAt := comments[len(comments)-1], time.Time{}
	for _, c := range comments {
		at, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil || !at.After(newestAt) {
			continue
		}
		newest, newestAt = c, at
	}
	return newest, true
}

// questionAgeSec is how long ago a question was asked, nil for a bead that
// records no time it can be measured from.
func questionAgeSec(now, askedAt time.Time) *int64 {
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

// questionSections reads a description's "## Heading" sections, keyed by the
// lowercased heading so a differently capitalised one still parses. A
// description the overseer's tool did not shape yields whatever headings it
// does carry, and the row is drawn from those.
func questionSections(description string) map[string]string {
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
		if name, ok := questionHeading(line); ok {
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

// questionHeading reads a "## Question" line as its heading name, lowercased.
// A line with more than two hashes is a deeper heading the description does not
// define, so it is body text.
func questionHeading(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "##")
	if !ok || strings.HasPrefix(rest, "#") {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(rest)), true
}

// questionField is one parsed field as the pane draws it: one line, with every
// control and invisible format character gone — the pane writes these fields
// with textContent, and a rune that reorders what the operator reads is as much
// a hazard as a tag — and capped at questionFieldMax runes.
func questionField(s string) string {
	flat := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	return capRunes(strings.Join(strings.Fields(flat), " "), questionFieldMax)
}

// questionHeldIDs are the bead ids a ## Held work section names. The section is
// prose the overseer wrote, so only tokens shaped like a bead id are kept: an
// id the page draws is one the operator can paste into bd.
func questionHeldIDs(body string) []string {
	var out []string
	for _, token := range strings.FieldsFunc(body, func(r rune) bool {
		return !(unicode.IsLower(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_')
	}) {
		token = strings.Trim(token, "._-")
		if questionBeadID.MatchString(token) {
			out = append(out, token)
		}
	}
	return out
}

// capRunes truncates s to n runes. A cap on bytes would cut a multi-byte rune
// in half and hand the page an invalid one.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
