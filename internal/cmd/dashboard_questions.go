package cmd

import (
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/question"
)

// The question rows of the dashboard's Attention panel: the questions the
// overseer has filed for Sloan, so he can see what is waiting on him and answer
// it from a terminal. It reads the
// town's own beads database, which is where a question is filed — a reader
// that listed from a rig worktree would see that rig's beads alone.
//
// A read that fails is a named state rather than an empty list, because "the
// overseer has asked nothing" and "the answer could not be read" are different
// things to tell the operator.
//
// The label, the description's sections and the [overseer] comment convention
// are internal/question's; gt ask, gt answer and the daemon's question
// collector read the same ones.

// dashQuestionRows is how many questions the pane draws. A longer queue is
// counted in More rather than truncated silently, and the rows past the cap
// cost no read of their own.
const dashQuestionRows = 20

// dashQuestionsReader reads the town's open overseer questions. list and
// comments are injectable so the row building is testable without a database.
type dashQuestionsReader struct {
	list     func() ([]*beads.Issue, error)
	comments func(id string) ([]beads.Comment, error)
	// all lifts the pane's row cap, for gt questions, which lists the whole
	// queue.
	all bool
}

func newDashQuestionsReader(townRoot string) *dashQuestionsReader {
	return newQuestionsReader(beads.New(beads.ResolveBeadsDir(townRoot)))
}

// questionReads is the part of the beads client the reader needs.
type questionReads interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Comments(id string) ([]beads.Comment, error)
}

func newQuestionsReader(b questionReads) *dashQuestionsReader {
	return &dashQuestionsReader{
		list:     func() ([]*beads.Issue, error) { return b.List(question.ListOptions) },
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
	if !r.all && len(candidates) > dashQuestionRows {
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
		c.row.AgeSec = question.AgeSec(now, c.askedAt)
		out.Rows = append(out.Rows, c.row)
	}
	return out
}

// questionCandidate is one listed bead and the time the queue orders it by.
type questionCandidate struct {
	row     dashboard.QuestionRow
	askedAt time.Time // zero when the bead's creation time does not parse
}

// questionRow is one listed bead as the pane draws it, before its comments are
// read.
func questionRow(issue *beads.Issue) questionCandidate {
	f := question.Parse(issue)
	return questionCandidate{
		row: dashboard.QuestionRow{
			ID:          issue.ID,
			Title:       f.Title,
			Priority:    issue.Priority,
			Question:    f.Question,
			Recommended: f.Recommended,
			Held:        f.Held,
			HeldCount:   issue.DependentCount,
			Effort:      question.EffortOf(issue.Labels),
			Malformed:   f.Malformed,
		},
		askedAt: question.AskedAt(issue),
	}
}

// sortQuestionRows orders the queue as a priority queue, the way the work queue
// is ordered: the most urgent priority first, and within a priority the
// question that has waited longest, so a teardown go read outranks a question
// asked yesterday. A bead with no time it can be measured from comes after
// every one that has a time, at its own priority: an unreadable creation time
// must not sort as the longest wait.
func sortQuestionRows(candidates []questionCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.row.Priority != b.row.Priority {
			return a.row.Priority < b.row.Priority
		}
		switch {
		case a.askedAt.IsZero() != b.askedAt.IsZero():
			return b.askedAt.IsZero()
		case a.askedAt.IsZero():
			return false
		}
		return a.askedAt.Before(b.askedAt)
	})
}

// questionState reads a question's standing off its newest comment.
func questionState(comments []beads.Comment) string {
	if question.Answered(comments) {
		return dashboard.QuestionAnswered
	}
	return dashboard.QuestionAwaiting
}
