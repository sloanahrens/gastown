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
	// Urgency orders the rows inside one owner and kind, lowest first: an
	// escalation's severity, a question's priority, an item's alert level.
	Urgency int `json:"-"`
}

// Attention is the panel's payload: everything waiting on a decision or an
// action, the human's rows and the overseer's in one list, each tagged with its
// owner so a reader of either role sees the whole of what is waiting (gt-bi6w0).
//
// It is built from the readers that already exist rather than read again: the
// escalation beads, the overseer's questions, and the attention queue's items
// as the alerter holds them. A read that failed says so here, because an empty
// list of what is waiting and a list that could not be read are different
// things to tell the operator.
type Attention struct {
	Rows []AttentionRow `json:"rows"`
	// More counts the rows the readers capped out of Rows.
	More int `json:"more,omitempty"`
	// Mine is how many of Rows the human owns, which is the number the strip's
	// "needs you" fact draws. A non-zero More makes it a floor rather than a
	// whole: the owner of a row the reader capped out is what that read never
	// took, so the strip draws the count as "N+" rather than as a number that
	// leaves the capped rows out in silence.
	Mine int `json:"mine"`
	// Failed names the readers behind the list that could not be read
	// ("escalations", "questions").
	Failed []string `json:"failed,omitempty"`
	// Pending names the readers that have not reported yet.
	Pending []string `json:"pending,omitempty"`
}

// buildAttention merges the three readers into the one list. items are the
// attention alerts that stand now (Alerter.Standing), now is the read time the
// items' ages are taken against.
func buildAttention(esc *Escalations, ask *Questions, items []Alert, now time.Time) *Attention {
	at := &Attention{
		Rows:   []AttentionRow{},
		Failed: failedReads(esc, ask),
	}
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
			Urgency: levelUrgency(al.Level),
		}
		if age := now.Sub(al.At); !al.At.IsZero() && age >= 0 {
			sec := int64(age.Seconds())
			row.AgeSec = &sec
		}
		at.Rows = append(at.Rows, row)
	}

	sort.SliceStable(at.Rows, func(i, j int) bool { return attentionBefore(at.Rows[i], at.Rows[j]) })
	for _, r := range at.Rows {
		if r.Owner == string(attention.OwnerSloan) {
			at.Mine++
		}
	}
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
// answered), then the loudest first and, at one loudness, the longest wait
// first.
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
