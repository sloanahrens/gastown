package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/rules"
)

// RulesBattery is the rules battery's name, its --battery value.
const RulesBattery = "rules"

// ruleWindow is how long after a finished session a guard record counts
// against it. A line younger than the window stays unlabeled, since a record
// may still be coming, and once the window has passed with no record the line
// is labeled not broken.
const ruleWindow = 20 * time.Minute

// The rules that label a rules line, by the name --json reports.
const (
	ruleRulesRevertGuard = "rules:revert-guard"
	ruleRulesStandDown   = "rules:rework-stand-down"
	ruleRulesNoRecord    = "rules:no-guard-record"
)

// reworkStandDownPrefix opens the close-skipped comment gt done writes when a
// rework stands down because its head is the rejected one (gt-3e1z4). The
// comment text is the record: no structured file holds it.
const reworkStandDownPrefix = "DONE_CLOSE_SKIPPED: head unchanged since the rejection"

// The question ids the rules battery logs, named by the rules package.
const (
	qBrokeStandingRule QuestionID = rules.QBrokeStandingRule
	qRuleKind          QuestionID = rules.QRuleKind
)

func init() {
	Register(RulesBattery, func(townRoot string) Adapter {
		stores := &townNotes{town: townRoot, stores: map[string]*beads.Beads{}}
		return &rulesAdapter{
			town: townRoot,
			comments: func(rig, bead string) ([]beads.Comment, error) {
				return beadComments(stores, rig, bead)
			},
			lines: map[string]rules.Line{},
			now:   time.Now,
		}
	})
}

// rulesAdapter labels each logged session from the guard records the town
// keeps for its bead: the revert guard's refusal ledger, and the close-skipped
// comments of a rework stand-down. It reads and never writes.
type rulesAdapter struct {
	town string
	// comments reads a bead's comments in its rig.
	comments func(rig, bead string) ([]beads.Comment, error)
	// lines holds each parsed line by item ID, so Label has the line's bead,
	// rig and submission time.
	lines map[string]rules.Line
	now   func() time.Time

	// refusals is the revert guard's ledger, read once on the first label.
	refusals     []attention.Refusal
	refusalsRead bool
	// sessionsRead counts the sessions whose bead comments Label read.
	sessionsRead int
}

func (a *rulesAdapter) Battery() string { return RulesBattery }
func (a *rulesAdapter) LogPath() string { return rules.LogPath(a.town) }

// rulesID names one logged session: its bead and the moment it was submitted,
// so a bead resubmitted after a refusal is its own item.
func rulesID(l rules.Line) string {
	return l.Bead + "@" + l.DoneAt.UTC().Format(time.RFC3339Nano)
}

// Parse reads one rules line. A session that was not asked has no answers, and
// one from an older question set cannot be compared with this one, so both are
// skipped and counted.
func (a *rulesAdapter) Parse(raw []byte) (Item, error) {
	var l rules.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Rig == "" || l.Bead == "" || strings.ContainsAny(l.Bead, `/\`) || l.TS.IsZero() || l.DoneAt.IsZero() {
		return Item{}, errors.New("rules line needs a rig, a bead, a ts and a done_at")
	}
	if l.QuestionVersion != rules.Version {
		return Item{}, fmt.Errorf("rules line asked question set %d, the scorecard reads %d", l.QuestionVersion, rules.Version)
	}
	if l.Answers == nil {
		return Item{}, fmt.Errorf("rules line has no answers: %s", l.Reason)
	}
	it := Item{
		ID:      rulesID(l),
		Time:    l.TS,
		Model:   l.Model,
		Answers: make(map[QuestionID]Answer, len(l.Answers)),
	}
	for id, ans := range l.Answers {
		it.Answers[QuestionID(id)] = Answer{Noul: ans.Noul, Choice: ans.Choice, Probabilities: ans.Probabilities}
	}
	a.lines[it.ID] = l
	return it, nil
}

// Label finds the labels for one logged session. A session younger than the
// window is unlabeled. Otherwise, a guard record for its bead inside the window
// after the submission labels it broke_standing_rule true. A window that has
// passed with no record labels it false. The rule that set the label is named
// with each source that matched, joined by "+".
//
// Only broke_standing_rule is labeled. rule_kind has no guard record that
// names a kind, so it is never labeled here.
func (a *rulesAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the rules adapter", item.ID)
	}
	if a.now().Sub(line.DoneAt) < ruleWindow {
		return Labeling{}, nil
	}
	refusals, err := a.readRefusals()
	if err != nil {
		return Labeling{}, err
	}
	comments, err := a.comments(line.Rig, line.Bead)
	if err != nil {
		return Labeling{}, fmt.Errorf("reading the comments of %s: %w", line.Bead, err)
	}
	a.sessionsRead++

	end := line.DoneAt.Add(ruleWindow)
	var sources []string
	for _, ref := range refusals {
		if ref.Kind == attention.KindRevertGuard && ref.Bead == line.Bead && inRuleWindow(ref.TS, line.DoneAt, end) {
			sources = append(sources, ruleRulesRevertGuard)
			break
		}
	}
	for _, c := range comments {
		at, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err != nil {
			continue
		}
		if strings.HasPrefix(c.Text, reworkStandDownPrefix) && inRuleWindow(at, line.DoneAt, end) {
			sources = append(sources, ruleRulesStandDown)
			break
		}
	}

	if len(sources) == 0 {
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{qBrokeStandingRule: false}, Rule: ruleRulesNoRecord}, nil
	}
	return Labeling{Labeled: true, Labels: map[QuestionID]bool{qBrokeStandingRule: true}, Rule: strings.Join(sources, "+")}, nil
}

// Context says which guard sources the labels were read from, and which rule
// has no record. The report prints these lines beside the metrics.
func (a *rulesAdapter) Context() []string {
	return []string{
		fmt.Sprintf("rules: guard sources read: revert-guard refusals (refusals.jsonl), %d record(s); rework stand-down comments, for %d session(s)",
			len(a.refusals), a.sessionsRead),
		"rules: BLOCKED mail is not read: no code path writes a record for it",
		fmt.Sprintf("rules: %s not-yet: no guard record names the rule a session broke", qRuleKind),
	}
}

// readRefusals returns the revert guard's ledger, read once.
func (a *rulesAdapter) readRefusals() ([]attention.Refusal, error) {
	if a.refusalsRead {
		return a.refusals, nil
	}
	refusals, err := attention.ReadRefusals(a.town)
	if err != nil {
		return nil, fmt.Errorf("reading the refusal ledger: %w", err)
	}
	a.refusals = refusals
	a.refusalsRead = true
	return refusals, nil
}

// inRuleWindow reports whether at falls from the submission to the window's end.
func inRuleWindow(at, from, end time.Time) bool {
	return !at.Before(from) && !at.After(end)
}

// beadComments reads a bead's comments from its rig's store. A bead the store
// does not hold has no comments.
func beadComments(stores *townNotes, rig, bead string) ([]beads.Comment, error) {
	b, err := stores.store(rig)
	if err != nil {
		return nil, err
	}
	cs, err := b.Comments(bead)
	if errors.Is(err, beads.ErrNotFound) {
		return nil, nil
	}
	return cs, err
}
