package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/question"
)

// attentionQuestionStale is how long a question may stay open before it is a
// question.stale item. The overseer's questions are for Sloan, who may not look
// for a day; past a day the overseer is told, as info, that the work they hold
// is still held.
const attentionQuestionStale = 24 * time.Hour

// questionReads are the outside reads the question collectors make: the open
// questions, and one question's comments. Both go to the town database, where
// a question is filed.
type questionReads struct {
	list     func(ctx context.Context) ([]*beads.Issue, error)
	comments func(ctx context.Context, id string) ([]beads.Comment, error)
}

// questionSnapshot is one tick's read of the open questions: each bead with
// its comments, so the three collectors judge the same picture.
type questionSnapshot struct {
	questions []openQuestion
}

// openQuestion is one open question bead and its comments.
type openQuestion struct {
	issue    *beads.Issue
	comments []beads.Comment
}

// readQuestions lists the open questions and reads each one's comments. Any
// failed read fails the snapshot: the collectors keep their previous items
// rather than raise "new" for a question whose comments could not be read.
func (r questionReads) readQuestions(ctx context.Context) (*questionSnapshot, error) {
	issues, err := r.list(ctx)
	if err != nil {
		return nil, err
	}
	snap := &questionSnapshot{}
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		comments, err := r.comments(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", issue.ID, err)
		}
		snap.questions = append(snap.questions, openQuestion{issue: issue, comments: comments})
	}
	return snap, nil
}

// commentReader is a work-bead reader that can also read a bead's comments,
// which the beads client does and a work-bead reader need not.
type commentReader interface {
	workBeadReader
	Comments(id string) ([]beads.Comment, error)
}

// productionQuestionReads reads the town database through bd pinned read-only,
// the way the escalation collector does, so the queue never writes to beads.
func (d *Daemon) productionQuestionReads() questionReads {
	b := d.workBeads(bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(d.config.TownRoot)), attentionBeadTimeout)
	return questionReads{
		list: func(ctx context.Context) ([]*beads.Issue, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return b.List(question.ListOptions)
		},
		comments: func(ctx context.Context, id string) ([]beads.Comment, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			c, ok := b.(commentReader)
			if !ok {
				return nil, fmt.Errorf("the work-bead reader cannot read comments")
			}
			return c.Comments(id)
		},
	}
}

// questionCollectors are the three question collectors, sharing one read of the
// open questions. The read is made by whichever collector runs first, and a
// failure of it fails all three alike. It is per call, so a tick that builds
// its collectors afresh never judges last tick's questions.
func (s *attentionSources) questionCollectors() []attentionCollector {
	var (
		snap *questionSnapshot
		err  error
		done bool
	)
	read := func(ctx context.Context) (*questionSnapshot, error) {
		if !done {
			snap, err = s.questions.readQuestions(ctx)
			done = true
		}
		return snap, err
	}
	collector := func(kind attention.Kind, items func(*questionSnapshot, time.Time) []attention.Item) attentionCollector {
		return attentionCollector{kind: kind, collect: func(ctx context.Context) ([]attention.Item, error) {
			snap, err := read(ctx)
			if err != nil {
				return nil, err
			}
			return items(snap, s.now), nil
		}}
	}
	return []attentionCollector{
		collector(attention.KindQuestionNew, questionNewItems),
		collector(attention.KindQuestionAnswered, questionAnsweredItems),
		collector(attention.KindQuestionStale, questionStaleItems),
	}
}

// questionNewItems raises question.new for each open question nobody has
// commented on: a question appeared. It clears with the first comment, the
// overseer's note or Sloan's answer, since from then on the question is being
// handled.
func questionNewItems(snap *questionSnapshot, _ time.Time) []attention.Item {
	var out []attention.Item
	for _, q := range snap.questions {
		if len(q.comments) > 0 {
			continue
		}
		out = append(out, questionItem(attention.KindQuestionNew, "new", q.issue, attention.SeverityHigh,
			"new question: "+questionTitle(q.issue)))
	}
	return out
}

// questionAnsweredItems raises question.answered for each open question whose
// newest comment is not the overseer's: Sloan has answered, and the overseer
// has yet to act on it. It clears when the overseer comments, and the daemon
// says it again every attention.RepeatEvery until then.
func questionAnsweredItems(snap *questionSnapshot, _ time.Time) []attention.Item {
	var out []attention.Item
	for _, q := range snap.questions {
		if !question.Answered(q.comments) {
			continue
		}
		out = append(out, questionItem(attention.KindQuestionAnswered, "answered", q.issue, attention.SeverityHigh,
			"answered, awaiting the overseer: "+questionTitle(q.issue)))
	}
	return out
}

// questionStaleItems raises question.stale for each question open longer than
// attentionQuestionStale, answered or not. A bead whose creation time does not
// parse has no age to judge and is skipped.
func questionStaleItems(snap *questionSnapshot, now time.Time) []attention.Item {
	var out []attention.Item
	for _, q := range snap.questions {
		askedAt := question.AskedAt(q.issue)
		if askedAt.IsZero() || now.Sub(askedAt) < attentionQuestionStale {
			continue
		}
		out = append(out, questionItem(attention.KindQuestionStale, "stale", q.issue, attention.SeverityLow,
			fmt.Sprintf("open %s: %s", questionOpenFor(now.Sub(askedAt)), questionTitle(q.issue))))
	}
	return out
}

// questionItem is one question's item: the key names the rule's word and the
// bead, so each rule's item is its own and an ack hides one rule only.
func questionItem(kind attention.Kind, word string, issue *beads.Issue, sev attention.Severity, summary string) attention.Item {
	return attention.Item{
		Key:      "question:" + word + ":" + issue.ID,
		Kind:     kind,
		Severity: sev,
		Bead:     issue.ID,
		Summary:  fmt.Sprintf("%s (%s)", summary, issue.ID),
	}
}

// questionTitle is a question's title as an item summary draws it.
func questionTitle(issue *beads.Issue) string {
	return question.CapRunes(question.Field(issue.Title), 120)
}

// questionOpenFor is how long a stale question has been open, in whole hours
// up to two days and whole days after.
func questionOpenFor(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
