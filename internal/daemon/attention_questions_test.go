package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/question"
)

// questionLedger is a synthetic ledger of open questions and their comments,
// which the fixture's sources read.
type questionLedger struct {
	issues   []*beads.Issue
	comments map[string][]beads.Comment
	err      error
}

func (l *questionLedger) reads() questionReads {
	return questionReads{
		list: func(context.Context) ([]*beads.Issue, error) { return l.issues, l.err },
		comments: func(_ context.Context, id string) ([]beads.Comment, error) {
			return l.comments[id], nil
		},
	}
}

func (l *questionLedger) ask(id, title string, at time.Time) {
	l.issues = append(l.issues, &beads.Issue{
		ID: id, Title: title, CreatedAt: at.Format(time.RFC3339),
		Description: "## Question\nq\n## Recommended answer\nr",
	})
}

func (l *questionLedger) comment(id, text string, at time.Time) {
	if l.comments == nil {
		l.comments = map[string][]beads.Comment{}
	}
	l.comments[id] = append(l.comments[id], beads.Comment{Text: text, CreatedAt: at.Format(time.RFC3339)})
}

// questionItems runs the three question collectors against the ledger at now
// and returns their items by kind.
func questionItems(t *testing.T, f *attentionFixture, now time.Time) map[attention.Kind][]attention.Item {
	t.Helper()
	f.src.now = now
	out := map[attention.Kind][]attention.Item{}
	for _, c := range f.src.questionCollectors() {
		out[c.kind] = f.collect(t, c.collect)
	}
	return out
}

func TestQuestionCollectorRaisesNewForAQuestionNobodyHasCommentedOn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	l := &questionLedger{}
	l.ask("hq-q1", "retry policy", now.Add(-time.Minute))
	l.ask("hq-q2", "gate order", now.Add(-time.Hour))
	l.comment("hq-q2", question.OverseerPrefix+" noted", now.Add(-30*time.Minute))
	f.src.questions = l.reads()

	items := questionItems(t, f, now)[attention.KindQuestionNew]
	if len(items) != 1 {
		t.Fatalf("new items = %+v, want the one question with no comment", items)
	}
	got := items[0]
	if got.Key != "question:new:hq-q1" || got.Bead != "hq-q1" || got.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want question:new:hq-q1 at high severity", got)
	}
	if rule := attention.RuleFor(got); rule != attention.RuleQuestionNew {
		t.Errorf("rule = %q, want %q", rule, attention.RuleQuestionNew)
	}
}

func TestQuestionCollectorRaisesAnsweredUntilTheOverseerComments(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	l := &questionLedger{}
	l.ask("hq-q1", "retry policy", now.Add(-2*time.Hour))
	l.comment("hq-q1", question.OverseerPrefix+" asked", now.Add(-110*time.Minute))
	l.comment("hq-q1", "yes, once", now.Add(-5*time.Minute))
	f.src.questions = l.reads()

	items := questionItems(t, f, now)[attention.KindQuestionAnswered]
	if len(items) != 1 || items[0].Key != "question:answered:hq-q1" {
		t.Fatalf("answered items = %+v, want hq-q1", items)
	}

	l.comment("hq-q1", question.OverseerPrefix+" acting on it", now.Add(-time.Minute))
	if items := questionItems(t, f, now)[attention.KindQuestionAnswered]; len(items) != 0 {
		t.Errorf("answered items = %+v, want none once the overseer has commented", items)
	}
}

func TestQuestionCollectorRaisesStalePastADay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	l := &questionLedger{}
	l.ask("hq-old", "old question", now.Add(-25*time.Hour))
	l.ask("hq-young", "young question", now.Add(-23*time.Hour))
	l.issues = append(l.issues, &beads.Issue{ID: "hq-notime", Title: "no creation time"})
	f.src.questions = l.reads()

	items := questionItems(t, f, now)[attention.KindQuestionStale]
	if len(items) != 1 || items[0].Key != "question:stale:hq-old" || items[0].Severity != attention.SeverityLow {
		t.Fatalf("stale items = %+v, want hq-old at low severity", items)
	}
}

// A read that fails keeps the queue as it was: no collector clears or raises on
// a question whose comments could not be read.
func TestQuestionCollectorsKeepTheirItemsWhenTheReadFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	l := &questionLedger{}
	l.ask("hq-q1", "retry policy", now.Add(-time.Minute))
	f.src.questions = l.reads()
	st := f.tick(t, now)
	if _, ok := attention.Find(st, "question:new:hq-q1"); !ok {
		t.Fatalf("state = %+v, want the new item", st.Items)
	}

	l.err = errors.New("dolt: connection refused")
	st = f.tick(t, now.Add(time.Minute))
	if _, ok := attention.Find(st, "question:new:hq-q1"); !ok {
		t.Errorf("state = %+v, want the item kept through a failed read", st.Items)
	}
	for _, e := range f.events(t) {
		if e.State == attention.EventCleared {
			t.Errorf("a failed read cleared %+v", e)
		}
	}
}

// Through a full tick: the three rules write their flags, and the answered
// rule says itself again no more than every 20 minutes until the overseer
// comments.
func TestQuestionRulesFireThroughTheTick(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, t0)
	l := &questionLedger{}
	l.ask("hq-new", "just asked", t0.Add(-time.Minute))
	l.ask("hq-ans", "answered one", t0.Add(-3*time.Hour))
	l.comment("hq-ans", question.OverseerPrefix+" asked", t0.Add(-170*time.Minute))
	l.comment("hq-ans", "go ahead", t0.Add(-10*time.Minute))
	l.ask("hq-old", "stale one", t0.Add(-30*time.Hour))
	l.comment("hq-old", question.OverseerPrefix+" asked", t0.Add(-29*time.Hour))
	f.src.questions = l.reads()

	f.tick(t, t0)
	raised := map[string]attention.Event{}
	for _, e := range f.events(t) {
		raised[e.Key] = e
	}
	for key, want := range map[string]struct {
		rule string
		wake bool
		hand attention.Handled
	}{
		"question:new:hq-new":      {attention.RuleQuestionNew, true, attention.HandledOperatorAction},
		"question:answered:hq-ans": {attention.RuleQuestionAnswered, true, attention.HandledOperatorAction},
		"question:stale:hq-old":    {attention.RuleQuestionStale, false, attention.HandledInfo},
	} {
		e, ok := raised[key]
		if !ok || e.Rule != want.rule || e.Wake != want.wake || e.Handled != want.hand || e.State != attention.EventNew {
			t.Errorf("event for %s = %+v (found %v), want %s wake=%v %s", key, e, ok, want.rule, want.wake, want.hand)
		}
	}

	countRepeats := func(key string) int {
		n := 0
		for _, e := range f.events(t) {
			if e.Key == key && e.State == attention.EventNew {
				n++
			}
		}
		return n
	}
	for _, min := range []int{5, 19} {
		f.tick(t, t0.Add(time.Duration(min)*time.Minute))
	}
	if n := countRepeats("question:answered:hq-ans"); n != 1 {
		t.Fatalf("answered lines after 19 minutes = %d, want only the raised one", n)
	}
	f.tick(t, t0.Add(20*time.Minute))
	f.tick(t, t0.Add(25*time.Minute))
	if n := countRepeats("question:answered:hq-ans"); n != 2 {
		t.Errorf("answered lines after 25 minutes = %d, want the raised line and one repeat", n)
	}
	if n := countRepeats("question:stale:hq-old"); n != 1 {
		t.Errorf("stale lines after 25 minutes = %d, want one (it repeats every 6h)", n)
	}
	f.tick(t, t0.Add(6*time.Hour+time.Minute))
	if n := countRepeats("question:stale:hq-old"); n != 2 {
		t.Errorf("stale lines after 6h = %d, want one repeat", n)
	}

	// The overseer comments: the answered item clears and stops repeating.
	l.comment("hq-ans", question.OverseerPrefix+" acting", t0.Add(7*time.Hour))
	before := countRepeats("question:answered:hq-ans")
	f.tick(t, t0.Add(7*time.Hour+time.Minute))
	f.tick(t, t0.Add(8*time.Hour))
	if n := countRepeats("question:answered:hq-ans"); n != before {
		t.Errorf("answered lines grew from %d to %d after the overseer commented", before, n)
	}
}

// The tick records when it said a repeating item, on the sources the daemon
// carries into its next tick.
func TestQuestionRepeatClockIsRecordedByTheTick(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, t0)
	l := &questionLedger{}
	l.ask("hq-ans", "answered one", t0.Add(-3*time.Hour))
	l.comment("hq-ans", "go ahead", t0.Add(-10*time.Minute))
	f.src.questions = l.reads()

	f.tick(t, t0)
	if got := f.src.repeatedAt["question:answered:hq-ans"]; !got.Equal(t0) {
		t.Errorf("repeat clock = %v, want %v", got, t0)
	}
}
