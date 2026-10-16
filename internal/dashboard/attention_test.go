package dashboard

import (
	"regexp"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// attentionNow is the instant the fixtures below are aged against.
var attentionNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func ageOfSec(sec int64) *int64 { return &sec }

// The panel is one list: the escalation beads, the open questions and the
// attention queue's standing items, each row tagged with the owner who has to
// move rather than split into a panel per reader (gt-bi6w0).
func TestBuildAttentionMergesTheReadersIntoOneTaggedList(t *testing.T) {
	t.Parallel()

	esc := &Escalations{
		Rows: []EscalationRow{
			{ID: "hq-1", Title: "disk full", Severity: "critical", EscalatedBy: "gastown/polecats/opal", AgeSec: ageOfSec(600)},
			{ID: "hq-2", Title: "which rig", Severity: "low", EscalatedBy: "overseer", AgeSec: ageOfSec(60)},
		},
		More: 1,
	}
	ask := &Questions{
		Rows: []QuestionRow{
			{ID: "gt-q1", Title: "retry policy", Priority: 0, Recommended: "retry twice", HeldCount: 2,
				Held: []string{"gt-h1"}, Effort: "quick", AgeSec: ageOfSec(3600), State: QuestionAwaiting},
			{ID: "gt-q2", Title: "split the bead", Priority: 3, AgeSec: ageOfSec(60), State: QuestionAnswered},
		},
		More: 2,
	}
	items := []Alert{
		{ID: "alert-1", At: attentionNow.Add(-30 * time.Minute), Level: LevelWarn, Key: "stall:gastown/opal",
			Title: attention.RulePolecatStall, Text: "gastown/opal is stalled", Rule: attention.RulePolecatStall},
		{ID: "alert-2", At: attentionNow.Add(-10 * time.Minute), Level: LevelCrit, Key: "host:disk",
			Title: attention.RuleHostDiskCrit, Text: "the volume is 96% full", Rule: attention.RuleHostDiskCrit},
	}

	at := buildAttention(esc, ask, items, attentionNow)
	if len(at.Failed) != 0 || len(at.Pending) != 0 {
		t.Errorf("a list built from reads that reported names %v failed and %v pending", at.Failed, at.Pending)
	}
	if at.More != 3 {
		t.Errorf("the list counts %d rows its readers capped out, want the escalations' 1 and the questions' 2", at.More)
	}

	// The human's rows first — a question above the other kinds, which is the
	// row type that stops the town until it is answered — then the overseer's,
	// and inside each group the loudest first and the longest wait behind it.
	want := []struct{ kind, owner, id string }{
		{AttentionQuestion, string(attention.OwnerSloan), "gt-q1"},     // P0, and the human's
		{AttentionEscalation, string(attention.OwnerSloan), "hq-2"},    // the human raised it
		{AttentionItem, string(attention.OwnerSloan), "host:disk"},     // the host is the human's
		{AttentionQuestion, string(attention.OwnerOverseer), "gt-q2"},  // answered: the overseer owes the next move
		{AttentionEscalation, string(attention.OwnerOverseer), "hq-1"}, // a polecat raised it
		{AttentionItem, string(attention.OwnerOverseer), "stall:gastown/opal"},
	}
	if len(at.Rows) != len(want) {
		t.Fatalf("the list holds %d rows (%+v), want %d", len(at.Rows), at.Rows, len(want))
	}
	for i, w := range want {
		if got := at.Rows[i]; got.Kind != w.kind || got.Owner != w.owner || got.ID != w.id {
			t.Errorf("row %d is %s/%s/%s, want %s/%s/%s", i, got.Kind, got.Owner, got.ID, w.kind, w.owner, w.id)
		}
	}
	if at.Mine != 3 {
		t.Errorf("the list counts %d rows for the human, want 3", at.Mine)
	}

	// The question's row carries what answering it costs, from the payload the
	// reader already built rather than a second join (gt-0ft4p).
	q := at.Rows[0]
	if q.Priority != 0 || q.Recommended != "retry twice" || q.HeldCount != 2 || q.Effort != "quick" || q.State != QuestionAwaiting {
		t.Errorf("the question row is %+v, want the priority, recommendation, held count, effort and state the reader sent", q)
	}
	if len(q.Held) != 1 || q.Held[0] != "gt-h1" {
		t.Errorf("the question row holds %v, want the bead the description names", q.Held)
	}
	// An item's age is the read time against the alert's own, and its title is
	// the queue's summary line rather than the rule id.
	item := at.Rows[2]
	if item.AgeSec == nil || *item.AgeSec != 600 {
		t.Errorf("the item is %d seconds old, want the 10 minutes since it was raised", *item.AgeSec)
	}
	if item.Title != "the volume is 96% full" || item.Rule != attention.RuleHostDiskCrit {
		t.Errorf("the item is %+v, want the summary line titled and the rule named", item)
	}
}

// A rejection is a feed line the alerter notices, not a condition the queue
// holds: it is the Feed's, where the landing worker logged it twice already,
// and the panel lists only what stands (gt-bi6w0).
func TestARejectionIsNotARowOfThePanel(t *testing.T) {
	t.Parallel()

	a := NewAlerter(func() time.Time { return alertBase })
	a.Observe(State{}, State{}, []Entry{{Seq: 1, At: alertBase, Kind: "landings",
		Text: "[land] gt-9: rejected (review): om requested changes"}})

	if at := buildAttention(nil, nil, a.Standing(), alertBase); len(at.Rows) != 0 {
		t.Errorf("a rejection reached the attention list: %+v", at.Rows)
	}
}

// A queue item of a rule the bead readers draw a richer row for is not listed
// again as a summary line: the escalation and the question are already rows,
// with the raiser, the recommended answer and the work they hold (gt-bi6w0).
func TestAttentionDoesNotListABeadTwice(t *testing.T) {
	t.Parallel()

	esc := &Escalations{Rows: []EscalationRow{{ID: "hq-1", Title: "disk full", Severity: "high"}}}
	ask := &Questions{Rows: []QuestionRow{{ID: "gt-q1", Title: "retry?", State: QuestionAwaiting}}}
	items := []Alert{
		{Key: "esc:hq-1", Rule: attention.RuleEscalation, Title: attention.RuleEscalation, Text: "an escalation is open", At: attentionNow},
		{Key: "ask:gt-q1", Rule: attention.RuleQuestionNew, Title: attention.RuleQuestionNew, Text: "a question is open", At: attentionNow},
		{Key: "stall:gastown/opal", Rule: attention.RulePolecatStall, Title: attention.RulePolecatStall, Text: "opal is stalled", At: attentionNow},
	}

	at := buildAttention(esc, ask, items, attentionNow)
	if len(at.Rows) != 3 {
		t.Fatalf("the list holds %d rows (%+v), want the bead's two and the queue's one", len(at.Rows), at.Rows)
	}
	for _, r := range at.Rows {
		if r.ID == "esc:hq-1" || r.ID == "ask:gt-q1" {
			t.Errorf("row %+v repeats a bead the escalation and question rows already carry", r)
		}
	}
}

// A read that failed, and a read that has not reported, are named rather than
// drawn as a list with nothing in it: "nothing is waiting" and "the list could
// not be read" are different things to tell the operator (gt-bi6w0).
func TestBuildAttentionNamesTheReadsBehindTheList(t *testing.T) {
	t.Parallel()

	failed := buildAttention(&Escalations{Unavailable: true}, &Questions{}, nil, attentionNow)
	if len(failed.Failed) != 1 || failed.Failed[0] != "escalations" {
		t.Errorf("a failed escalations read names %v", failed.Failed)
	}
	if n := buildAttention(&Escalations{}, &Questions{Unavailable: true}, nil, attentionNow); len(n.Failed) != 1 || n.Failed[0] != "questions" {
		t.Errorf("a failed questions read names %v", n.Failed)
	}
	both := buildAttention(nil, nil, nil, attentionNow)
	if len(both.Pending) != 2 || both.Pending[0] != "escalations" || both.Pending[1] != "questions" {
		t.Errorf("neither reader having reported names %v pending", both.Pending)
	}
	if both.Rows == nil {
		t.Error("a list with no reads behind it is nil, which the page cannot tell from no list at all")
	}

	// The rows the reader that did report returned are listed beside the name of
	// the one that failed, and only those are counted for the human.
	half := buildAttention(&Escalations{}, &Questions{Rows: []QuestionRow{{ID: "gt-q1", State: QuestionAwaiting}}}, nil, attentionNow)
	if len(half.Rows) != 1 || half.Mine != 1 {
		t.Errorf("a half-read list holds %+v with %d for the human, want the one question", half.Rows, half.Mine)
	}
}

// The owner table is the one place the human's rows and the overseer's are told
// apart, and the page tags a row with the owner it returns. Every rule the
// attention queue can raise must therefore have an owner the page draws a tag
// and a chip for: a rule owned by something no chip carries is a row the reader
// cannot filter (gt-bi7w0's panel, gt-71714's table).
func TestEveryAttentionRuleHasAnOwnerThePageDraws(t *testing.T) {
	t.Parallel()

	tags := attentionOwnerTags(t)
	if len(tags) != 2 || tags[0] != string(attention.OwnerSloan) || tags[1] != string(attention.OwnerOverseer) {
		t.Fatalf("the page builds its chips from %v, want the two owners", tags)
	}
	for _, rule := range attention.Rules() {
		if owner := string(attention.RuleOwner(rule)); !contains(tags, owner) {
			t.Errorf("rule %q is owned by %q, which the page has no tag for", rule, owner)
		}
	}
}

// attentionOwnerTags reads the owner tags the page builds the panel's chips
// from: the one list of owners a row can be tagged with.
func attentionOwnerTags(t *testing.T) []string {
	t.Helper()

	m := regexp.MustCompile(`const OWNERS = \[([^\]]*)\];`).FindStringSubmatch(string(indexHTML))
	if m == nil {
		t.Fatal("index.html has no OWNERS table")
	}
	var out []string
	for _, q := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	return out
}
