package dashboard

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
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
			{ID: "gt-q1", Title: "retry policy", Priority: 0, Question: "retry?", Recommended: "retry twice", HeldCount: 2,
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

	at := buildAttention(esc, ask, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
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
		{AttentionQuestion, string(attention.OwnerOperator), "gt-q1"},  // P0, and the human's
		{AttentionEscalation, string(attention.OwnerOperator), "hq-2"}, // the human raised it
		{AttentionItem, string(attention.OwnerOperator), "host:disk"},  // the host is the human's
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

	// The question's row carries the question's own words and what answering it
	// costs, from the payload the reader already built rather than a second
	// join (gt-0ft4p).
	q := at.Rows[0]
	if q.Priority != 0 || q.Question != "retry?" || q.Recommended != "retry twice" ||
		q.HeldCount != 2 || q.Effort != "quick" || q.State != QuestionAwaiting {
		t.Errorf("the question row is %+v, want the question, the priority, recommendation, held count, effort and state the reader sent", q)
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

// gt-zevjx: the review queue does not read as an incident. A risk-path item the
// queue raised at review debt's severity draws the calm chip — the level the
// alerts carry is what the page colours and ranks by — and it sorts behind the
// human's question and behind the daemon's own alarms.
func TestAttentionPanelSortsTheAlarmsAboveReviewDebt(t *testing.T) {
	t.Parallel()

	const debtText = "gt-abc landed deadbeef touching internal/daemon/attention.go; overseer review FAILED"

	// The levels are the ones the daemon's severities map to, so the rows here
	// are the rows its events raise. The debt row's level comes from the rule
	// that decides it rather than being written beside it, so a rule that goes
	// back to raising every risk-path item loud has to fail this test (gt-i6j2n).
	items := []Alert{
		{ID: "alert-1", At: attentionNow.Add(-time.Hour), Key: "risk:gt-abc:deadbeef",
			Level: attentionLevel(attention.RiskPathSeverity(attention.RiskPathEvidence{Failed: true, Priority: 3})),
			Title: attention.RuleRiskPath, Text: debtText,
			Rule: attention.RuleRiskPath},
		{ID: "alert-2", At: attentionNow.Add(-2 * time.Minute), Level: attentionLevel(attention.SeverityHigh), Key: "red-main:gastown:internal/cmd",
			Title: attention.RuleRedMain, Text: "internal/cmd is red on main", Rule: attention.RuleRedMain},
		{ID: "alert-3", At: attentionNow.Add(-time.Minute), Level: attentionLevel(attention.SeverityHigh), Key: "host.docker_stack_down",
			Title: attention.RuleHostDockerDown, Text: "3 of 4 stack containers not running", Rule: attention.RuleHostDockerDown},
	}
	ask := &Questions{Rows: []QuestionRow{{ID: "gt-q1", Title: "retry policy", Priority: 0, State: QuestionAwaiting}}}

	at := buildAttention(nil, ask, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	want := []string{"gt-q1", "host.docker_stack_down", "red-main:gastown:internal/cmd", "risk:gt-abc:deadbeef"}
	if len(at.Rows) != len(want) {
		t.Fatalf("the list holds %+v, want the question and the three queue items", at.Rows)
	}
	for i, id := range want {
		if at.Rows[i].ID != id {
			t.Fatalf("row %d is %q, want %q (%+v)", i, at.Rows[i].ID, id, at.Rows)
		}
	}

	debt := at.Rows[len(at.Rows)-1]
	for _, row := range at.Rows[1 : len(at.Rows)-1] {
		if row.Level != LevelCrit {
			t.Errorf("the alarm %q draws level %q, want the crit it is", row.ID, row.Level)
		}
		if row.Urgency >= debt.Urgency {
			t.Errorf("the alarm %q ranks %d against the debt's %d, want it louder", row.ID, row.Urgency, debt.Urgency)
		}
	}
	if debt.Level != LevelWarn {
		t.Errorf("the review-debt row draws level %q, want the warn its low severity maps to", debt.Level)
	}
	// The page colours a queue row from the level the row carries, so the level
	// above is the chip the operator reads: drawn through the page's own script
	// rather than matched in its source (page_harness_test.go, gt-i6j2n).
	raw, err := json.Marshal(at)
	if err != nil {
		t.Fatalf("marshalling the panel's rows: %v", err)
	}
	reading := map[string]any{}
	if err := json.Unmarshal(raw, &reading); err != nil {
		t.Fatalf("reading the panel's rows back: %v", err)
	}
	dom := newPageDOM(t)
	dom.call(t, "renderAttention", attentionState(reading))
	switchOwner(t, dom, string(attention.OwnerOverseer))
	drawn := panelRows(t, dom)
	found := false
	for _, row := range drawn {
		if !strings.Contains(row.Text, debtText) {
			continue
		}
		found = true
		if !panelCells(row, "span alvl "+LevelWarn) {
			t.Errorf("the review-debt row wears %v, want the %s chip its level draws", row.Cells, LevelWarn)
		}
	}
	if !found {
		t.Errorf("the panel drew no row for the risk-path item: %+v", drawn)
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

	if at := buildAttention(nil, nil, a.Standing(), AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, alertBase); len(at.Rows) != 0 {
		t.Errorf("a rejection reached the attention list: %+v", at.Rows)
	}
}

// A bead rejected twice is routine rework, so the row it raises is the
// overseer's and never one of the human's: the line the daemon writes for the
// item is the one the panel reads, and the owner that line carries keeps the
// row out of the count the strip draws red. The rule keeps its wake, so the row
// is still there for the overseer to act on (gt-few2w, gt-2f1k2).
func TestARejectedTwiceRowIsTheOverseersAndNotTheHumans(t *testing.T) {
	t.Parallel()

	// The line the daemon writes for the condition: the item's own new
	// transition, carrying the rule its kind maps to and that rule's flags.
	res := attention.Reconcile(attention.State{}, []attention.Item{{
		Key: "rejected-twice:gt-9", Kind: attention.KindRejectedTwice,
		Severity: attention.SeverityHigh, Summary: "gt-9 was rejected twice",
	}}, attention.Acks{}, attentionNow)
	if len(res.Events) != 1 {
		t.Fatalf("the item raised %d transitions (%+v), want the one line the panel reads", len(res.Events), res.Events)
	}

	a := NewAlerter(func() time.Time { return attentionNow })
	raised, _ := a.ObserveAttention(res.Events, attention.Acks{})
	if len(raised) != 1 {
		t.Fatalf("the line raised %+v, want the one row: a rule that wakes still lists it", raised)
	}

	at := buildAttention(&Escalations{}, &Questions{}, a.Standing(), AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(at.Rows) != 1 {
		t.Fatalf("the panel holds %+v, want the rejected bead's own row", at.Rows)
	}
	if r := at.Rows[0]; r.Owner != string(attention.OwnerOverseer) || r.Rule != attention.RuleRejectedTwice {
		t.Errorf("the rejected bead's row is %s under %q, want it the overseer's under %q",
			r.Owner, r.Rule, attention.RuleRejectedTwice)
	}
	if at.Mine != 0 {
		t.Errorf("the panel counts %d rows for the human, want none: a second rejection is routine rework", at.Mine)
	}
	if got := stripNeedsYouAlarm(at); got != "" {
		t.Errorf("the strip alarms %q for the human, want nothing: the row is the overseer's own", got)
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

	at := buildAttention(esc, ask, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(at.Rows) != 3 {
		t.Fatalf("the list holds %d rows (%+v), want the bead's two and the queue's one", len(at.Rows), at.Rows)
	}
	for _, r := range at.Rows {
		if r.ID == "esc:hq-1" || r.ID == "ask:gt-q1" {
			t.Errorf("row %+v repeats a bead the escalation and question rows already carry", r)
		}
	}
}

// The human's share is a floor when a reader capped rows out of the list: the
// owner of a capped row is what that read never took, so the count the strip
// draws says so rather than leaving the capped rows out in silence (gt-bi6w0).
func TestAttentionCountsTheHumanRowsBesideTheCap(t *testing.T) {
	t.Parallel()

	at := buildAttention(
		&Escalations{Rows: []EscalationRow{{ID: "hq-1", Title: "disk full", Severity: "high"}}, More: 4},
		&Questions{Rows: []QuestionRow{{ID: "gt-q1", Title: "retry?", State: QuestionAwaiting}}, More: 1},
		nil, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow,
	)
	if at.Mine != 1 || at.More != 5 {
		t.Errorf("the list counts %d rows for the human beside %d capped out, want the one awaiting question and both caps",
			at.Mine, at.More)
	}
}

// A read that failed, and a read that has not reported, are named rather than
// drawn as a list with nothing in it: "nothing is waiting" and "the list could
// not be read" are different things to tell the operator (gt-bi6w0).
func TestBuildAttentionNamesTheReadsBehindTheList(t *testing.T) {
	t.Parallel()

	failed := buildAttention(&Escalations{Unavailable: true}, &Questions{}, nil, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(failed.Failed) != 1 || failed.Failed[0] != "escalations" {
		t.Errorf("a failed escalations read names %v", failed.Failed)
	}
	if n := buildAttention(&Escalations{}, &Questions{Unavailable: true}, nil, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow); len(n.Failed) != 1 || n.Failed[0] != "questions" {
		t.Errorf("a failed questions read names %v", n.Failed)
	}
	// A queue state that could not be read is not an empty queue: the rows still
	// stand, and the read that would have classified them is named.
	if n := buildAttention(&Escalations{}, &Questions{}, nil, AttentionQueue{Failed: true}, ScoresRead{}, LandingRiskRead{}, attentionNow); len(n.Failed) != 1 || n.Failed[0] != "attention queue" {
		t.Errorf("a failed queue read names %v", n.Failed)
	}
	both := buildAttention(nil, nil, nil, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(both.Pending) != 2 || both.Pending[0] != "escalations" || both.Pending[1] != "questions" {
		t.Errorf("neither reader having reported names %v pending", both.Pending)
	}
	if both.Rows == nil {
		t.Error("a list with no reads behind it is nil, which the page cannot tell from no list at all")
	}

	// The rows the reader that did report returned are listed beside the name of
	// the one that failed, and only those are counted for the human.
	half := buildAttention(&Escalations{}, &Questions{Rows: []QuestionRow{{ID: "gt-q1", State: QuestionAwaiting}}}, nil, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(half.Rows) != 1 || half.Mine != 1 {
		t.Errorf("a half-read list holds %+v with %d for the human, want the one question", half.Rows, half.Mine)
	}
}

// The panel's rows are split by class into the two groups it draws: what needs
// a person and what is only waiting on work already in flight. The split is
// read off the queue's own items, so a pipeline row names who holds it and what
// it waits on, while a row the queue does not hold is act (gt-tv5v2).
func TestBuildAttentionSplitsTheRowsIntoNeedsYouAndPipeline(t *testing.T) {
	t.Parallel()

	items := []Alert{
		{ID: "alert-1", At: attentionNow.Add(-time.Minute), Level: LevelCrit, Key: "host:disk",
			Title: attention.RuleHostDiskCrit, Text: "the volume is 96% full", Rule: attention.RuleHostDiskCrit},
		{ID: "alert-2", At: attentionNow.Add(-2 * time.Minute), Level: LevelWarn, Key: "rejected-twice:gt-9",
			Title: attention.RuleRejectedTwice, Text: "gt-9 was rejected twice", Rule: attention.RuleRejectedTwice},
		{ID: "alert-3", At: attentionNow.Add(-3 * time.Minute), Level: LevelWarn, Key: "risk:gt-7:deadbeef",
			Title: attention.RuleRiskPath, Text: "gt-7 landed touching internal/cmd", Rule: attention.RuleRiskPath},
		{ID: "alert-4", At: attentionNow.Add(-4 * time.Minute), Level: LevelWarn, Key: "review:backlog",
			Title: attention.RuleReviewBacklog, Text: "the review queue is long", Rule: attention.RuleReviewBacklog},
	}
	// The queue holds the three items it knows; the risk item's owner is enough
	// to make it pipeline with no waiting-on line, and the review backlog is
	// pipeline with neither.
	queue := []attention.Item{
		{Key: "rejected-twice:gt-9", Kind: attention.KindRejectedTwice, Owner: "opal", WaitingOn: "rework by opal, attempt 2"},
		{Key: "risk:gt-7:deadbeef", Kind: attention.KindRiskPath, Owner: "opal"},
		{Key: "review:backlog", Kind: attention.KindReviewBacklog},
	}

	at := buildAttention(&Escalations{}, &Questions{}, items, AttentionQueue{Items: queue}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if at.NeedsYou.Count != 1 || len(at.NeedsYou.Rows) != 1 {
		t.Errorf("the needs-you group holds %d rows (%+v), want the host reading alone", at.NeedsYou.Count, at.NeedsYou.Rows)
	}
	if at.Pipeline.Count != 3 || len(at.Pipeline.Rows) != 3 {
		t.Errorf("the pipeline group holds %d rows (%+v), want the three items in flight", at.Pipeline.Count, at.Pipeline.Rows)
	}
	if got := at.NeedsYou.Rows[0].ID; got != "host:disk" {
		t.Errorf("the needs-you group leads with %q, want the host reading", got)
	}

	rows := map[string]AttentionRow{}
	for _, r := range at.Rows {
		rows[r.ID] = r
	}
	if r := rows["host:disk"]; r.Class != string(attention.ClassAct) || r.HeldBy != "" || r.WaitingOn != "" {
		t.Errorf("the host row is class %q held by %q waiting on %q, want act with neither", r.Class, r.HeldBy, r.WaitingOn)
	}
	if r := rows["rejected-twice:gt-9"]; r.Class != string(attention.ClassPipeline) ||
		r.HeldBy != "opal" || r.WaitingOn != "rework by opal, attempt 2" {
		t.Errorf("the rejected-twice row is class %q held by %q waiting on %q, want the queue's owner and line",
			r.Class, r.HeldBy, r.WaitingOn)
	}
	if r := rows["risk:gt-7:deadbeef"]; r.Class != string(attention.ClassPipeline) || r.HeldBy != "opal" || r.WaitingOn != "" {
		t.Errorf("the risk row is class %q held by %q waiting on %q, want pipeline with its owner alone",
			r.Class, r.HeldBy, r.WaitingOn)
	}
	if r := rows["review:backlog"]; r.Class != string(attention.ClassPipeline) {
		t.Errorf("the review-backlog row is class %q, want pipeline", r.Class)
	}
}

// The strip's count is the human's rows that need a person and nothing else. A
// pipeline row clears without them, so one the human's rule owns is left out of
// both the count and the alarm; a load that counts it is red with nothing for
// them to do (gt-tv5v2).
func TestMineCountsOnlyTheRowsThatNeedAPerson(t *testing.T) {
	t.Parallel()

	// host.gpu.hot is a rule the owner table has no row for, so it reads by its
	// family as the human's — the way a rule a newer daemon writes reaches the
	// human's panel — and the queue holds it as a review backlog, the kind that
	// is work in flight whenever somebody owns it.
	pipeOnly := []Alert{
		{Key: "host:gpu", Rule: "host.gpu.hot", Text: "the GPU is hot", At: attentionNow, Level: LevelWarn},
	}
	queue := AttentionQueue{Items: []attention.Item{
		{Key: "host:gpu", Kind: attention.KindReviewBacklog, Owner: "opal", WaitingOn: "the next review round"},
	}}
	at := buildAttention(&Escalations{}, &Questions{}, pipeOnly, queue, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(at.Pipeline.Rows) != 1 || at.Pipeline.Rows[0].Owner != string(attention.OwnerOperator) {
		t.Fatalf("the fixture drew %+v in the pipeline group, want one row the human's rule owns", at.Pipeline.Rows)
	}
	if at.Mine != 0 {
		t.Errorf("the list counts %d rows for the human, want none: the human's one row is in the pipeline", at.Mine)
	}
	if got := stripNeedsYouAlarm(at); got != "" {
		t.Errorf("the strip alarms %q with only a pipeline row standing, want no alarm", got)
	}

	// The row that does need a person, beside it, is what the count is made of.
	withAct := append(append([]Alert{}, pipeOnly...),
		Alert{Key: "host:disk", Rule: attention.RuleHostDiskCrit, Text: "the volume is 96% full", At: attentionNow, Level: LevelCrit})
	at = buildAttention(&Escalations{}, &Questions{}, withAct, queue, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(at.NeedsYou.Rows) != 1 || at.Pipeline.Count != 1 {
		t.Fatalf("the groups hold %+v and %+v, want the host reading and the GPU row", at.NeedsYou.Rows, at.Pipeline.Rows)
	}
	if at.Mine != 1 {
		t.Errorf("the list counts %d rows for the human, want the one reading that needs a person", at.Mine)
	}
}

// A standing row whose item the queue no longer holds is act: the panel cannot
// see what the item waits on, and a condition it could not classify is shown
// rather than filed away (gt-tv5v2).
func TestBuildAttentionShowsARowTheQueueCannotClassify(t *testing.T) {
	t.Parallel()

	items := []Alert{
		{Key: "rejected-twice:gt-9", Rule: attention.RuleRejectedTwice, Title: attention.RuleRejectedTwice,
			Text: "gt-9 was rejected twice", At: attentionNow, Level: LevelWarn},
	}
	at := buildAttention(&Escalations{}, &Questions{}, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if len(at.Rows) != 1 || at.Rows[0].Class != string(attention.ClassAct) {
		t.Fatalf("the list holds %+v, want the row shown as act", at.Rows)
	}
	if at.NeedsYou.Count != 1 || at.Pipeline.Count != 0 {
		t.Errorf("the groups hold %d and %d rows, want the row in the needs-you group", at.NeedsYou.Count, at.Pipeline.Count)
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
	if len(tags) != 2 || tags[0] != string(attention.OwnerOperator) || tags[1] != string(attention.OwnerOverseer) {
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

// The dashboard serves the owner tag as the row's owner, and the page's chip
// and item badge read that word, so the JSON a reader gets names the operator:
// the owner's own value, never a person's name (gt-rujis).
func TestTheServedAttentionNamesTheOperator(t *testing.T) {
	t.Parallel()

	// A host reading: a rule the owner table gives the operator.
	items := []Alert{
		{Key: "host:disk", Rule: attention.RuleHostDiskCrit, Text: "the volume is 96% full", At: attentionNow, Level: LevelCrit},
	}
	at := buildAttention(&Escalations{}, &Questions{}, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	raw, err := json.Marshal(at)
	if err != nil {
		t.Fatalf("marshalling the attention reading: %v", err)
	}
	if want := `"owner":"operator"`; !strings.Contains(string(raw), want) {
		t.Errorf("the served attention carries no %s: %s", want, raw)
	}
	if strings.Contains(string(raw), "sloan") {
		t.Errorf("the served attention still names the old owner: %s", raw)
	}
}

// The owner tag is drawn in the colour its .own-<owner> rule gives it, so the
// operator's tag needs that rule under its own name. Without it the tag falls
// back to the default colour and reads as an ordinary label (gt-rujis).
func TestTheOperatorsTagHasItsColour(t *testing.T) {
	t.Parallel()

	if want := ".own-" + string(attention.OwnerOperator) + "{"; !strings.Contains(string(indexHTML), want) {
		t.Errorf("index.html has no %s rule, so the operator's tag is drawn in the default colour", want)
	}
}

// Inside one alert level the queue items are ordered by Jev's needs_human
// answer, highest first, and every item stays in the list with its score beside
// it: the score orders the queue and clears nothing from it.
func TestAttentionOrdersItemsByNeedsHumanScore(t *testing.T) {
	t.Parallel()

	items := []Alert{
		{ID: "alert-1", At: attentionNow.Add(-3 * time.Minute), Level: LevelWarn, Key: "red-main:gastown",
			Title: attention.RuleRedMain, Text: "main is red", Rule: attention.RuleRedMain},
		{ID: "alert-2", At: attentionNow.Add(-2 * time.Minute), Level: LevelWarn, Key: "stall:gastown/opal",
			Title: attention.RulePolecatStall, Text: "opal is stalled", Rule: attention.RulePolecatStall},
		{ID: "alert-3", At: attentionNow.Add(-time.Minute), Level: LevelWarn, Key: "landing:gt-1",
			Title: attention.RuleLandingStuck, Text: "a landing is stuck", Rule: attention.RuleLandingStuck},
	}
	scores := attention.Scores{
		attention.LifeKey("red-main:gastown", items[0].At):   0.20,
		attention.LifeKey("stall:gastown/opal", items[1].At): 0.90,
		attention.LifeKey("landing:gt-1", items[2].At):       0.50,
	}

	at := buildAttention(nil, nil, items, AttentionQueue{}, ScoresRead{Scores: scores}, LandingRiskRead{}, attentionNow)
	if len(at.Rows) != 3 {
		t.Fatalf("the list holds %d rows (%+v), want all three items", len(at.Rows), at.Rows)
	}
	want := []struct {
		id    string
		score float64
	}{
		{"stall:gastown/opal", 0.90},
		{"landing:gt-1", 0.50},
		{"red-main:gastown", 0.20},
	}
	for i, w := range want {
		row := at.Rows[i]
		if row.ID != w.id {
			t.Fatalf("row %d is %q, want %q (rows %+v)", i, row.ID, w.id, at.Rows)
		}
		if row.NeedsHuman == nil || *row.NeedsHuman != w.score {
			t.Errorf("row %q shows the score %v, want %.2f", row.ID, row.NeedsHuman, w.score)
		}
	}
}

// The alert level leads the score: a crit row is never buried under a warn one,
// however low Jev scores it, because this is the page that raises the alarms.
func TestAttentionKeepsACritRowAboveAWarnOne(t *testing.T) {
	t.Parallel()

	items := []Alert{
		{ID: "alert-1", At: attentionNow.Add(-3 * time.Minute), Level: LevelCrit, Key: "red-main:gastown",
			Title: attention.RuleRedMain, Text: "main is red", Rule: attention.RuleRedMain},
		{ID: "alert-2", At: attentionNow.Add(-2 * time.Minute), Level: LevelWarn, Key: "stall:gastown/opal",
			Title: attention.RulePolecatStall, Text: "opal is stalled", Rule: attention.RulePolecatStall},
		{ID: "alert-3", At: attentionNow.Add(-time.Minute), Level: LevelCrit, Key: "host.docker_stack_down",
			Title: attention.RuleHostDiskCrit, Text: "the volume is 96% full", Rule: attention.RuleHostDiskCrit},
	}
	scores := attention.Scores{
		attention.LifeKey("red-main:gastown", items[0].At):       0.10,
		attention.LifeKey("stall:gastown/opal", items[1].At):     0.90,
		attention.LifeKey("host.docker_stack_down", items[2].At): 0.60,
	}

	at := buildAttention(nil, nil, items, AttentionQueue{}, ScoresRead{Scores: scores}, LandingRiskRead{}, attentionNow)
	want := []string{"host.docker_stack_down", "red-main:gastown", "stall:gastown/opal"}
	if len(at.Rows) != len(want) {
		t.Fatalf("the list holds %+v, want all three items", at.Rows)
	}
	for i, id := range want {
		if at.Rows[i].ID != id {
			t.Fatalf("row %d is %q, want %q (rows %+v)", i, at.Rows[i].ID, id, at.Rows)
		}
	}
}

// A tie in the score keeps arrival order, and an item Jev has not answered sorts
// after the scored ones without leaving the list.
func TestAttentionKeepsTiesInArrivalOrderAndUnscoredRowsVisible(t *testing.T) {
	t.Parallel()

	items := []Alert{
		{ID: "alert-1", At: attentionNow.Add(-5 * time.Minute), Level: LevelWarn, Key: "red-main:gastown",
			Title: attention.RuleRedMain, Text: "main is red", Rule: attention.RuleRedMain},
		{ID: "alert-2", At: attentionNow.Add(-4 * time.Minute), Level: LevelWarn, Key: "stall:gastown/opal",
			Title: attention.RulePolecatStall, Text: "opal is stalled", Rule: attention.RulePolecatStall},
		{ID: "alert-3", At: attentionNow.Add(-time.Minute), Level: LevelWarn, Key: "landing:gt-1",
			Title: attention.RuleLandingStuck, Text: "a landing is stuck", Rule: attention.RuleLandingStuck},
	}
	scores := attention.Scores{
		attention.LifeKey("red-main:gastown", items[0].At):   0.50,
		attention.LifeKey("stall:gastown/opal", items[1].At): 0.50,
	}

	at := buildAttention(nil, nil, items, AttentionQueue{}, ScoresRead{Scores: scores}, LandingRiskRead{}, attentionNow)
	want := []string{"red-main:gastown", "stall:gastown/opal", "landing:gt-1"}
	if len(at.Rows) != len(want) {
		t.Fatalf("the list holds %+v, want the tied pair and the unanswered item", at.Rows)
	}
	for i, id := range want {
		if at.Rows[i].ID != id {
			t.Fatalf("row %d is %q, want %q (%+v)", i, at.Rows[i].ID, id, at.Rows)
		}
	}
	if last := at.Rows[2]; last.NeedsHuman != nil {
		t.Errorf("the unanswered item shows the score %v, want none", *last.NeedsHuman)
	}
}

// The risk-path rows are ranked among each other by the landing battery's risk
// answer, highest first, inside their group of one owner, kind and alert level:
// the level still leads, so a crit review stays above a warn one, and no row
// leaves the list (gt-n8tmf).
func TestAttentionOrdersRiskPath(t *testing.T) {
	t.Parallel()

	const (
		riskA = "risk:gt-a:aaaaaaaaaaaa"
		riskB = "risk:gt-b:bbbbbbbbbbbb"
		riskC = "risk:gt-c:cccccccccccc"
		riskD = "risk:gt-d:dddddddddddd"
		riskE = "risk:gt-e:eeeeeeeeeeee"
	)
	riskPath := func(key, text string, at time.Time, level string) Alert {
		return Alert{ID: key, At: at, Level: level, Key: key, Title: attention.RuleRiskPath, Text: text, Rule: attention.RuleRiskPath}
	}
	items := []Alert{
		riskPath(riskA, "gt-a landed touching cmd", attentionNow.Add(-6*time.Minute), LevelWarn),
		riskPath(riskB, "gt-b landed touching cmd", attentionNow.Add(-5*time.Minute), LevelWarn),
		riskPath(riskC, "gt-c landed touching cmd", attentionNow.Add(-4*time.Minute), LevelWarn),
		riskPath(riskD, "gt-d landed touching cmd", attentionNow.Add(-3*time.Minute), LevelWarn),
		{ID: "alert-5", At: attentionNow.Add(-2 * time.Minute), Level: LevelWarn, Key: "stall:gastown/opal",
			Title: attention.RulePolecatStall, Text: "opal is stalled", Rule: attention.RulePolecatStall},
		riskPath(riskE, "gt-e landed touching guards", attentionNow.Add(-time.Minute), LevelCrit),
	}
	risks := attention.LandingRisks{riskA: 1, riskB: 3, riskC: 2, riskE: 0}

	at := buildAttention(nil, nil, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{Risks: risks}, attentionNow)
	if len(at.Rows) != len(items) {
		t.Fatalf("the panel holds %d rows, want all %d", len(at.Rows), len(items))
	}
	// The crit review leads however low its risk, and every warn row keeps its
	// place: the four warn risk-path rows are ranked by the landing answer and
	// the stall row keeps the place today's order gave it.
	want := []string{riskE, riskB, riskC, riskA, riskD, "stall:gastown/opal"}
	for i, id := range want {
		if at.Rows[i].ID != id {
			t.Fatalf("row %d is %q, want %q (rows %+v)", i, at.Rows[i].ID, id, at.Rows)
		}
	}
	for _, row := range at.Rows {
		switch row.ID {
		case riskE, riskA, riskB, riskC:
			risk, ok := risks[row.ID]
			if !ok || row.LandingRisk == nil || *row.LandingRisk != risk {
				t.Errorf("row %q shows the landing risk %v, want %.1f", row.ID, row.LandingRisk, risk)
			}
		case riskD, "stall:gastown/opal":
			if row.LandingRisk != nil {
				t.Errorf("row %q shows the landing risk %v, want none", row.ID, *row.LandingRisk)
			}
		}
	}
}

// A landing log that exists but could not be read keeps the last good risk
// answers, marks them stale, and names the failed read: the panel does not draw
// an old answer as this read's (gt-n8tmf).
func TestAttentionKeepsTheLastLandingRiskOnAFailedRead(t *testing.T) {
	t.Parallel()

	const riskA = "risk:gt-a:aaaaaaaaaaaa"
	items := []Alert{{
		ID: riskA, At: attentionNow.Add(-5 * time.Minute), Level: LevelWarn, Key: riskA,
		Title: attention.RuleRiskPath, Text: "gt-a landed touching cmd", Rule: attention.RuleRiskPath,
	}}
	at := buildAttention(nil, nil, items, AttentionQueue{}, ScoresRead{},
		LandingRiskRead{Risks: attention.LandingRisks{riskA: 2}, Err: errors.New("reading landing.jsonl: permission denied")},
		attentionNow)
	if !at.LandingRiskFailed {
		t.Error("a failed landing read left the panel saying the risk answers were read")
	}
	if len(at.Rows) != 1 || at.NeedsYou.Count != 1 {
		t.Fatalf("a failed landing read left rows %+v, %d needs-you", at.Rows, at.NeedsYou.Count)
	}
	row := at.Rows[0]
	if row.LandingRisk == nil || *row.LandingRisk != 2 {
		t.Errorf("the row shows the landing risk %v, want the last good answer 2", row.LandingRisk)
	}
	if !row.LandingRiskStale {
		t.Error("the row draws the last good answer as this read's")
	}
}

// Risk-path rows that tie on the landing risk keep the order the panel gave
// them: the risk ranks nothing between them, so the longest wait still leads,
// and an unanswered row sorts after the answered ones in its group. Every row
// stays on the list (gt-n8tmf).
func TestAttentionKeepsThePanelOrderOnALandingRiskTie(t *testing.T) {
	t.Parallel()

	const (
		riskOld = "risk:gt-a:aaaaaaaaaaaa"
		riskMid = "risk:gt-b:bbbbbbbbbbbb"
		riskNew = "risk:gt-c:cccccccccccc"
		riskNil = "risk:gt-d:dddddddddddd"
	)
	riskPath := func(key string, at time.Time) Alert {
		return Alert{ID: key, At: at, Level: LevelWarn, Key: key, Title: attention.RuleRiskPath,
			Text: key + " landed touching cmd", Rule: attention.RuleRiskPath}
	}
	// The panel's age order is nil, mid, old, new; the tie ranks the answered
	// three in that order and sends the unanswered one last.
	items := []Alert{
		riskPath(riskOld, attentionNow.Add(-4*time.Minute)),
		riskPath(riskNil, attentionNow.Add(-6*time.Minute)),
		riskPath(riskNew, attentionNow.Add(-3*time.Minute)),
		riskPath(riskMid, attentionNow.Add(-5*time.Minute)),
	}
	risks := attention.LandingRisks{riskOld: 2, riskMid: 2, riskNew: 2}

	at := buildAttention(nil, nil, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{Risks: risks}, attentionNow)
	want := []string{riskMid, riskOld, riskNew, riskNil}
	if len(at.Rows) != len(want) {
		t.Fatalf("the panel holds %d rows, want all %d", len(at.Rows), len(want))
	}
	for i, id := range want {
		if at.Rows[i].ID != id {
			t.Fatalf("row %d is %q, want %q (rows %+v)", i, at.Rows[i].ID, id, at.Rows)
		}
	}
}

// An answers log that exists but could not be read is a state of its own: the
// rows and the counts still stand, and the panel says the scores are missing
// rather than drawing a queue Jev answered nothing about (gt-b9j3o.2).
func TestAttentionNamesAnAnswersLogItCannotRead(t *testing.T) {
	t.Parallel()

	items := []Alert{
		{ID: "alert-1", At: attentionNow.Add(-3 * time.Minute), Level: LevelWarn, Key: "red-main:gastown",
			Title: attention.RuleRedMain, Text: "main is red", Rule: attention.RuleRedMain},
	}
	at := buildAttention(nil, nil, items, AttentionQueue{},
		ScoresRead{Err: errors.New("reading attention.jsonl: permission denied")}, LandingRiskRead{}, attentionNow)
	if !at.ScoresFailed {
		t.Error("a failed answers read left the panel saying the scores were read")
	}
	// The rows were read, so they are not a failed reader's: the groups and the
	// human's count keep standing.
	if len(at.Rows) != 1 || at.NeedsYou.Count != 1 || len(at.Failed) != 0 {
		t.Errorf("a failed answers read left rows %+v, %d needs-you and failed %v", at.Rows, at.NeedsYou.Count, at.Failed)
	}
	if at.Rows[0].NeedsHuman != nil {
		t.Errorf("the row shows the score %v for a log nobody could read", *at.Rows[0].NeedsHuman)
	}

	// A read with no error clears it again.
	ok := buildAttention(nil, nil, items, AttentionQueue{}, ScoresRead{}, LandingRiskRead{}, attentionNow)
	if ok.ScoresFailed {
		t.Error("a read with no error left the panel saying the scores were unreadable")
	}
}
