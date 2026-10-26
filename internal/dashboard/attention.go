package dashboard

import (
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// The row kinds: what a row of the Attention panel came from. They are the
// panel's own vocabulary — the page colors and orders by them — and never a
// statement about the reader behind the row.
const (
	// AttentionEscalation is an open escalation bead somebody raised.
	AttentionEscalation = "escalation"
	// AttentionQuestion is an open overseer question, the row type the panel
	// leads with: it is the one that stops the town while it waits.
	AttentionQuestion = "question"
	// AttentionItem is a condition the attention queue is holding, such as a
	// stalled landing or a host running out of disk.
	AttentionItem = "item"
)

// AttentionRow is one thing waiting on a decision or an action, as the page
// draws it. Every field is optional but Kind and Owner: an escalation carries a
// severity and a raiser, a question its priority and the answer recommended to
// it, and a queue item the rule that raised it.
type AttentionRow struct {
	// Kind is what the row came from (AttentionEscalation, AttentionQuestion,
	// AttentionItem).
	Kind string `json:"kind"`
	// Owner is who has to move: the human or the overseer (attention.Owner).
	Owner string `json:"owner"`
	// Rule is the attention rule the row is about, and the key of the owner
	// table that gave it its Owner. A question carries the rule of its state,
	// so an answered question reads as the overseer's.
	Rule string `json:"rule,omitempty"`
	// ID is the bead to comment on — the row's whole answer path, for a
	// question — or the queue item's own key for an item.
	ID string `json:"id,omitempty"`
	// Title is what the row is about in the raiser's own words.
	Title string `json:"title,omitempty"`
	// AgeSec is how long it has waited, nil when the reader could not parse the
	// time: an age of zero would read as "just now".
	AgeSec *int64 `json:"age_sec,omitempty"`
	// EscalatedBy is who raised an escalation, which is what its owner was read
	// off.
	EscalatedBy string `json:"escalated_by,omitempty"`
	// Severity and Level are the escalation's own severity and the loudness the
	// alerter gave a queue item. "" for a question.
	Severity string `json:"severity,omitempty"`
	Level    string `json:"level,omitempty"`
	// Priority is a question's bd priority, 0-4.
	Priority int `json:"priority,omitempty"`
	// Question is the question's own words, the description's ## Question
	// section, and Recommended the answer the overseer recommends to it;
	// HeldCount, Held and Effort are what answering it costs and what it
	// unblocks.
	Question    string   `json:"question,omitempty"`
	Recommended string   `json:"recommended,omitempty"`
	HeldCount   int      `json:"held_count,omitempty"`
	Held        []string `json:"held,omitempty"`
	Effort      string   `json:"effort,omitempty"`
	// State is a question's own state (QuestionAwaiting or QuestionAnswered),
	// and Malformed a question whose description carries neither section the
	// panel draws.
	State     string `json:"state,omitempty"`
	Malformed bool   `json:"malformed,omitempty"`
	// Class is whether the row needs a person (attention.ClassAct) or waits on
	// work already in flight (attention.ClassPipeline): the two groups the
	// panel draws. Every escalation and question is act; a queue item's class
	// is attention.Classify's, and an item the queue no longer holds is act, so
	// an unread condition is seen rather than filed away (gt-tv5v2).
	Class string `json:"class,omitempty"`
	// HeldBy is a queue item's own owner: who holds the condition now, which is
	// not the owner tag above (that is who has to move). Empty when the item
	// names nobody.
	HeldBy string `json:"held_by,omitempty"`
	// WaitingOn is a queue item's own waiting-on line: what clears it. Empty
	// when the item waits on nothing it can name.
	WaitingOn string `json:"waiting_on,omitempty"`
	// NeedsHuman is Jev's needs_human probability for a queue item: nil when no
	// answer names it yet, and for the row kinds the queue's battery does not
	// score. It ranks the item rows inside their alert level and is drawn
	// beside each one, so a low score sinks a row without hiding it.
	NeedsHuman *float64 `json:"needs_human,omitempty"`
	// Urgency orders the rows inside one owner and kind, lowest first: an
	// escalation's severity, a question's priority, an item's alert level.
	Urgency int `json:"-"`
}

// AttentionGroup is one half of the panel: the rows the panel draws under one
// heading, in the panel's own order, and how many they are. The count is the
// number the header prints, so a reader of the JSON does not have to count the
// rows to know what the header says (gt-tv5v2).
type AttentionGroup struct {
	Rows  []AttentionRow `json:"rows"`
	Count int            `json:"count"`
}

// Attention is the panel's payload: everything waiting on a decision or an
// action, the human's rows and the overseer's in one list, each tagged with its
// owner so a reader of either role sees the whole of what is waiting (gt-bi6w0).
//
// The same rows are also split by Class into the two groups the panel draws:
// NeedsYou, the rows that need a person, and Pipeline, the rows waiting on work
// already in flight (gt-tv5v2). Rows keeps the whole list in the panel's order
// for the readers that filter it by owner.
//
// It is built from the readers that already exist rather than read again: the
// escalation beads, the overseer's questions, and the attention queue's items
// as the alerter holds them. A read that failed says so here, because an empty
// list of what is waiting and a list that could not be read are different
// things to tell the operator.
type Attention struct {
	Rows []AttentionRow `json:"rows"`
	// NeedsYou is the group that needs a person, and Pipeline the group waiting
	// on work in flight: the two halves of Rows, each with the count the panel's
	// header prints.
	NeedsYou AttentionGroup `json:"needs_you"`
	Pipeline AttentionGroup `json:"pipeline"`
	// More counts the rows the readers capped out of Rows.
	More int `json:"more,omitempty"`
	// Mine is how many of the rows that need a person the human owns, which is
	// the number the strip's "needs you" fact draws. A pipeline row is left out
	// even when the human owns it: it clears without them, and counting it is
	// what leaves the pill loud with nothing for them to do (gt-tv5v2). A
	// non-zero More makes it a floor rather than a whole: the owner of a row the
	// reader capped out is what that read never took (stripNeedsYouAlarm).
	Mine int `json:"mine"`
	// Failed names the readers behind the list that could not be read
	// ("escalations", "questions", "attention queue").
	Failed []string `json:"failed,omitempty"`
	// ScoresFailed says Jev's answers log exists and could not be read. It is
	// not a failed row reader: the rows and their owners were read, so the
	// panel keeps its groups and counts and draws "?" where a score would be,
	// rather than claiming Jev answered nothing (gt-b9j3o.2).
	ScoresFailed bool `json:"scores_failed,omitempty"`
	// Pending names the readers that have not reported yet.
	Pending []string `json:"pending,omitempty"`
}

// AttentionQueue is what one read of the attention queue's state gave back: the
// items as the daemon last wrote them, and whether that read could be made at
// all (NewAttentionFollower.Items).
type AttentionQueue struct {
	Items []attention.Item
	// Failed says the state could not be read, which is not the same as a queue
	// holding nothing: the panel's rows still stand, and only the class, owner
	// and waiting-on they carry are missing (gt-tv5v2).
	Failed bool
}

// ScoresRead is one read of Jev's needs_human answers: the answers by item
// life, and the error of a log that exists but could not be read. An error is
// not an empty set of answers: the panel then names the read instead of
// drawing every row as unanswered.
type ScoresRead struct {
	Scores attention.Scores
	Err    error
}

// buildAttention merges the three readers into the one list. items are the
// attention alerts that stand now (Alerter.Standing), queue the attention
// queue's own items as the daemon last wrote them, scores the needs_human
// answers by item life, and now is the read time the items' ages are taken
// against.
//
// A standing alert is matched to its queue item by key for the class, owner and
// waiting-on the row carries. A key the queue does not hold — a cleared item
// whose line has not arrived, or a state nobody could read — is act, so a
// condition nobody could classify is seen rather than filed away. A read that
// failed says so in Failed beside the rows it could not classify.
func buildAttention(esc *Escalations, ask *Questions, items []Alert, queue AttentionQueue, scores ScoresRead, now time.Time) *Attention {
	byKey := make(map[string]attention.Item, len(queue.Items))
	for _, it := range queue.Items {
		byKey[it.Key] = it
	}
	at := &Attention{
		Rows:   []AttentionRow{},
		Failed: failedReads(esc, ask),
	}
	if queue.Failed {
		at.Failed = append(at.Failed, "attention queue")
	}
	at.ScoresFailed = scores.Err != nil
	if esc == nil {
		at.Pending = append(at.Pending, "escalations")
	}
	if ask == nil {
		at.Pending = append(at.Pending, "questions")
	}

	if esc != nil {
		at.More += esc.More
		for _, r := range esc.Rows {
			// Only the escalation itself says whether the human raised it; the
			// rule rows cannot (attention.EscalationOwner).
			at.Rows = append(at.Rows, AttentionRow{
				Kind:        AttentionEscalation,
				Owner:       string(attention.EscalationOwner(r.EscalatedBy)),
				Rule:        attention.RuleEscalation,
				ID:          r.ID,
				Title:       r.Title,
				AgeSec:      r.AgeSec,
				EscalatedBy: r.EscalatedBy,
				Severity:    r.Severity,
				Class:       string(attention.ClassAct),
				Urgency:     severityUrgency(r.Severity),
			})
		}
	}

	if ask != nil {
		at.More += ask.More
		for _, r := range ask.Rows {
			// An answered question is the overseer's to act on; awaiting is the
			// human's. The rule states it, so the row reads its owner off the
			// owner table like every other row.
			rule := attention.RuleQuestionNew
			if r.State == QuestionAnswered {
				rule = attention.RuleQuestionAnswered
			}
			at.Rows = append(at.Rows, AttentionRow{
				Kind:        AttentionQuestion,
				Owner:       string(attention.RuleOwner(rule)),
				Rule:        rule,
				ID:          r.ID,
				Title:       r.Title,
				AgeSec:      r.AgeSec,
				Priority:    r.Priority,
				Question:    r.Question,
				Recommended: r.Recommended,
				HeldCount:   r.HeldCount,
				Held:        r.Held,
				Effort:      r.Effort,
				State:       r.State,
				Malformed:   r.Malformed,
				Class:       string(attention.ClassAct),
				Urgency:     r.Priority,
			})
		}
	}

	for _, al := range items {
		if coveredByABead(al.Rule) {
			continue // the escalation or question row above already carries it
		}
		row := AttentionRow{
			Kind:    AttentionItem,
			Owner:   string(attention.RuleOwner(al.Rule)),
			Rule:    al.Rule,
			ID:      al.Key,
			Title:   al.Text,
			Level:   al.Level,
			Class:   string(attention.ClassAct),
			Urgency: levelUrgency(al.Level),
		}
		if it, ok := byKey[al.Key]; ok {
			row.Class = string(attention.Classify(it))
			row.HeldBy = it.Owner
			row.WaitingOn = it.WaitingOn
		}
		if age := now.Sub(al.At); !al.At.IsZero() && age >= 0 {
			sec := int64(age.Seconds())
			row.AgeSec = &sec
		}
		if score, ok := scores.Scores[attention.LifeKey(al.Key, al.At)]; ok {
			row.NeedsHuman = &score
		}
		at.Rows = append(at.Rows, row)
	}

	sort.SliceStable(at.Rows, func(i, j int) bool { return attentionBefore(at.Rows[i], at.Rows[j]) })
	at.NeedsYou = AttentionGroup{Rows: []AttentionRow{}}
	at.Pipeline = AttentionGroup{Rows: []AttentionRow{}}
	for _, r := range at.Rows {
		if r.Class == string(attention.ClassPipeline) {
			at.Pipeline.Rows = append(at.Pipeline.Rows, r)
			continue
		}
		at.NeedsYou.Rows = append(at.NeedsYou.Rows, r)
	}
	// Only the rows that need a person: a pipeline row the human owns clears
	// without them, and counting it would make the pill loud with nothing to do
	// (gt-tv5v2).
	for _, r := range at.NeedsYou.Rows {
		if r.Owner == string(attention.OwnerSloan) {
			at.Mine++
		}
	}
	at.NeedsYou.Count = len(at.NeedsYou.Rows)
	at.Pipeline.Count = len(at.Pipeline.Rows)
	return at
}

// coveredByABead reports whether a queue item's own rule is one the bead
// readers already draw a richer row for: an escalation item and a question item
// describe the bead the escalation and question rows carry, with the raiser,
// the recommended answer and the work it holds, which the queue's summary line
// does not.
func coveredByABead(rule string) bool {
	switch rule {
	case attention.RuleEscalation,
		attention.RuleQuestionNew,
		attention.RuleQuestionAnswered,
		attention.RuleQuestionStale:
		return true
	}
	return false
}

// attentionBefore orders the panel: the human's rows above the overseer's, the
// questions above the other kinds (a question stops the town until it is
// answered), then the loudest first. Inside one alert level of queue items,
// Jev's needs_human answer ranks them, highest first; an item with no answer
// sorts after the answered ones. At one score, and for the row kinds the
// battery does not score, the longest wait leads.
//
// The level leads because this is the page that raises alarms: a crit row must
// not sink under a warn one, so the score orders each level and never buries or
// hides a row (gt-b9j3o.2). The queue's own table has no alert levels and ranks
// by the score alone (attention.OrderByNeedsHuman).
func attentionBefore(a, b AttentionRow) bool {
	if a.Owner != b.Owner {
		return ownerRank(a.Owner) < ownerRank(b.Owner)
	}
	if a.Kind != b.Kind {
		return kindRank(a.Kind) < kindRank(b.Kind)
	}
	if a.Urgency != b.Urgency {
		return a.Urgency < b.Urgency
	}
	switch {
	case a.NeedsHuman != nil && b.NeedsHuman != nil:
		if *a.NeedsHuman != *b.NeedsHuman {
			return *a.NeedsHuman > *b.NeedsHuman
		}
	case a.NeedsHuman != nil:
		return true
	case b.NeedsHuman != nil:
		return false
	}
	return ageOf(a.AgeSec) > ageOf(b.AgeSec)
}

// ownerRank orders the owners a row can carry: the human first, then the
// overseer, and an owner this binary does not know last, where it is visible
// rather than hidden among the rows somebody is already reading.
func ownerRank(owner string) int {
	switch attention.Owner(owner) {
	case attention.OwnerSloan:
		return 0
	case attention.OwnerOverseer:
		return 1
	}
	return 2
}

// kindRank orders the row kinds.
func kindRank(kind string) int {
	switch kind {
	case AttentionQuestion:
		return 0
	case AttentionEscalation:
		return 1
	}
	return 2
}

// severityUrgency reads an escalation's severity as a rank, the loudest first.
// A severity this binary does not know is the middle of the four rather than
// the loudest or the quietest, where it is neither shouting nor buried.
func severityUrgency(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	}
	return 2
}

// levelUrgency reads an alert level as a rank, the loudest first.
func levelUrgency(level string) int {
	switch level {
	case LevelCrit:
		return 0
	case LevelWarn:
		return 1
	}
	return 2
}

// ageOf reads an age as a duration, an unreadable one as zero so it sorts last
// among rows of equal loudness.
func ageOf(sec *int64) time.Duration {
	if sec == nil {
		return 0
	}
	return time.Duration(*sec) * time.Second
}

// failedReads names the bead readers behind the list whose read failed.
func failedReads(esc *Escalations, ask *Questions) []string {
	var out []string
	if esc != nil && esc.Unavailable {
		out = append(out, "escalations")
	}
	if ask != nil && ask.Unavailable {
		out = append(out, "questions")
	}
	return out
}
