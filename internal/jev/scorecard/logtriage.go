package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

// LogtriageBattery is the daemon-log triage battery's name, its --battery value.
const LogtriageBattery = "logtriage"

// logtriageWindow is how long after a group's last line a follow-up can land
// and still count. A group younger than this is unlabeled: its window is open.
const logtriageWindow = time.Hour

// The rules that label a group line, by the name --json reports. A line that
// matches several joins their names with "+".
const (
	ruleLogtriageFollowUp   = "logtriage:follow-up-within-hour"
	ruleLogtriageQuiet      = "logtriage:no-follow-up-within-hour"
	ruleLogtriageSelfHealed = "logtriage:self-healed"
)

// logtriageBeadID matches a bead id in a daemon line. It is the same pattern
// the battery's pattern key uses, so a sample names the beads the key hides.
var logtriageBeadID = regexp.MustCompile(`\b(?:gt|hq|be|bd)-[a-z0-9]+(?:-[a-z0-9]+)*\b`)

func init() {
	Register(LogtriageBattery, func(townRoot string) Adapter {
		b := beads.New(townRoot)
		return newLogtriageAdapter(
			logtriage.LogPath(townRoot),
			func() ([]attention.Event, error) { return attention.ReadRetainedEvents(townRoot) },
			func() ([]*beads.Issue, error) { return beads.ListAllEscalationsAcrossRigs(b) },
			time.Now,
		)
	})
}

// logtriageAdapter labels each group line from what the town raised within an
// hour after the group's last daemon line. It reads and never writes. The
// events and escalations are loaded once, on the first Label that needs them.
type logtriageAdapter struct {
	log         string
	events      func() ([]attention.Event, error)
	escalations func() ([]*beads.Issue, error)
	now         func() time.Time

	// lines holds each parsed line by item ID, so Label has the group's key,
	// category and samples.
	lines map[string]logtriage.Line

	loaded bool
	attn   []attention.Event
	esc    []logtriageEscalation

	// matchWord runs one subject word's pattern, for the test that counts which
	// texts reach it. nil runs the pattern's MatchString.
	matchWord func(*regexp.Regexp, string) bool
}

func newLogtriageAdapter(log string, events func() ([]attention.Event, error), escalations func() ([]*beads.Issue, error), now func() time.Time) *logtriageAdapter {
	return &logtriageAdapter{
		log:         log,
		events:      events,
		escalations: escalations,
		now:         now,
		lines:       map[string]logtriage.Line{},
	}
}

func (a *logtriageAdapter) Battery() string { return LogtriageBattery }
func (a *logtriageAdapter) LogPath() string { return a.log }

// logtriageID names one logged group: its pattern key and the moment the pass
// logged it, so the same key in a later pass is its own item.
func logtriageID(l logtriage.Line) string {
	return l.Key + "@" + l.TS.UTC().Format(time.RFC3339Nano)
}

// Parse reads one triage line. A group whose call failed has no answers, and
// one from an older question set cannot be compared with this one, so both are
// skipped and counted.
func (a *logtriageAdapter) Parse(raw []byte) (Item, error) {
	var l logtriage.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Key == "" || l.TS.IsZero() {
		return Item{}, errors.New("logtriage line needs a key and a ts")
	}
	if l.QuestionVersion != logtriage.Version {
		return Item{}, fmt.Errorf("logtriage line asked question set %d, the scorecard reads %d", l.QuestionVersion, logtriage.Version)
	}
	if l.Answers == nil {
		return Item{}, fmt.Errorf("logtriage line has no answers: %s", l.Reason)
	}
	// The group's time is its last daemon line, which is when the follow-up
	// window opens. A group with no timestamps falls back to when it was logged.
	at := l.Last
	if at.IsZero() {
		at = l.TS
	}
	it := Item{ID: logtriageID(l), Time: at, Model: l.Model, Answers: map[QuestionID]Answer{}}
	for id, ans := range l.Answers {
		switch id {
		case logtriage.QRoutineNoise, logtriage.QNeedsAction, logtriage.QAutoRemediate:
			it.Answers[QuestionID(id)] = Answer{Noul: ans.Noul}
		case logtriage.QSeverity:
			it.Answers[QuestionID(id)] = Answer{Score: ans.Score}
		}
	}
	a.lines[it.ID] = l
	return it, nil
}

// Label finds the labels for one logged group, judged from what the town
// raised within the hour after the group's last line:
//
//  1. an attention event that is not self_healed, or an escalation, whose text
//     shares the group's category word or a bead id from its samples, sets
//     needs_action true and is_routine_noise false;
//  2. an escalation also sets severity: true when it is critical or high (a
//     page), false otherwise (a review);
//  3. a self_healed attention event that shares the subject sets
//     auto_remediable true, and needs_action only if rule 1 also matched;
//  4. no matching follow-up sets needs_action false.
//
// A group whose last line is younger than an hour is unlabeled, because its
// window is still open. So is a group with no timestamp, which has no window.
// A group with no category and no bead id has nothing a follow-up could share,
// and a follow-up whose time is unreadable could fall in the window, so neither
// is labeled quiet.
func (a *logtriageAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the logtriage adapter", item.ID)
	}
	last := line.Last
	if last.IsZero() || a.now().Sub(last) < logtriageWindow {
		return Labeling{}, nil
	}
	if err := a.load(); err != nil {
		return Labeling{}, err
	}

	subject := newLogtriageSubject(line, a.matchWord)
	if len(subject.words) == 0 {
		return Labeling{}, nil
	}
	end := last.Add(logtriageWindow)
	inWindow := func(at time.Time) bool { return !at.Before(last) && !at.After(end) }

	var followUp, selfHealed, escalated, page, unreadable bool
	for _, e := range a.attn {
		// A readable time outside the window cannot be the follow-up, so it is
		// skipped before the pattern runs. A time that will not read could fall
		// inside, so it still matches first and sets unreadable.
		if !e.TS.IsZero() && !inWindow(e.TS) {
			continue
		}
		if !subject.matches(e.Text) {
			continue
		}
		if e.TS.IsZero() {
			unreadable = true
			continue
		}
		if e.Handled == attention.HandledSelfHealed {
			selfHealed = true
			continue
		}
		followUp = true
	}
	for _, e := range a.esc {
		if e.readable && !inWindow(e.at) {
			continue
		}
		if !subject.matchesLowered(e.title) {
			continue
		}
		if !e.readable {
			unreadable = true
			continue
		}
		followUp, escalated = true, true
		if isPageSeverity(e.severity) {
			page = true
		}
	}
	if unreadable && !followUp {
		return Labeling{}, nil
	}

	labels := map[QuestionID]bool{QuestionID(logtriage.QNeedsAction): followUp}
	var rules []string
	if followUp {
		labels[QuestionID(logtriage.QRoutineNoise)] = false
		rules = append(rules, ruleLogtriageFollowUp)
	} else {
		rules = append(rules, ruleLogtriageQuiet)
	}
	if escalated {
		labels[QuestionID(logtriage.QSeverity)] = page
	}
	if selfHealed {
		labels[QuestionID(logtriage.QAutoRemediate)] = true
		rules = append(rules, ruleLogtriageSelfHealed)
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// load reads the attention events and the escalation beads once, and shapes
// them for the per-group loop: a state that is not new is skipped there for
// every group, so it is dropped here, and an escalation's time, severity and
// lowercased title are read once rather than once per group (gt-f65wb).
func (a *logtriageAdapter) load() error {
	if a.loaded {
		return nil
	}
	events, err := a.events()
	if err != nil {
		return fmt.Errorf("reading the attention events: %w", err)
	}
	escalations, err := a.escalations()
	if err != nil {
		return fmt.Errorf("reading the escalation beads: %w", err)
	}
	a.attn = make([]attention.Event, 0, len(events))
	for _, e := range events {
		if e.State == attention.EventNew {
			a.attn = append(a.attn, e)
		}
	}
	a.esc = make([]logtriageEscalation, 0, len(escalations))
	for _, issue := range escalations {
		a.esc = append(a.esc, newLogtriageEscalation(issue))
	}
	a.loaded = true
	return nil
}

// logtriageEscalation is an escalation as Label reads it: its raised time when
// that reads, and the rest of what the loop needs, read once.
type logtriageEscalation struct {
	at       time.Time
	readable bool
	title    string
	severity string
}

// newLogtriageEscalation reads an escalation's raised time from its own
// escalated_at, or the bead's creation when the description has none.
func newLogtriageEscalation(issue *beads.Issue) logtriageEscalation {
	fields := beads.ParseEscalationFields(issue.Description)
	raw := fields.EscalatedAt
	if raw == "" {
		raw = issue.CreatedAt
	}
	at, err := time.Parse(time.RFC3339, raw)
	return logtriageEscalation{
		at:       at,
		readable: err == nil,
		title:    strings.ToLower(issue.Title),
		severity: fields.Severity,
	}
}

// logtriageSubject is the words a follow-up's text must share: the group's
// category word and each bead id its samples name. Each word is a pattern that
// matches it only as a whole token, so "dolt" does not match "doltish" and
// "gt-abc1" does not match "gt-abc12".
type logtriageSubject struct {
	words []*regexp.Regexp
	// matchWord runs one word's pattern. newLogtriageSubject fills it, so the
	// scan is one loop whether or not the adapter sets a counter.
	matchWord func(*regexp.Regexp, string) bool
}

func newLogtriageSubject(line logtriage.Line, matchWord func(*regexp.Regexp, string) bool) logtriageSubject {
	if matchWord == nil {
		matchWord = patternMatches
	}
	var words []string
	if c := line.Answers[logtriage.QCategory].Choice; c != "" {
		words = append(words, strings.ToLower(c))
	}
	seen := map[string]bool{}
	for _, sample := range line.Samples {
		for _, id := range logtriageBeadID.FindAllString(strings.ToLower(sample), -1) {
			if !seen[id] {
				seen[id] = true
				words = append(words, id)
			}
		}
	}
	subject := logtriageSubject{words: make([]*regexp.Regexp, 0, len(words)), matchWord: matchWord}
	for _, w := range words {
		subject.words = append(subject.words, regexp.MustCompile(`(?:^|[^a-z0-9_-])`+regexp.QuoteMeta(w)+`(?:$|[^a-z0-9_-])`))
	}
	return subject
}

// patternMatches runs a word's pattern. It is the subject's matchWord when the
// adapter sets no counter.
func patternMatches(re *regexp.Regexp, text string) bool { return re.MatchString(text) }

// matches reports whether text contains one of the subject's words as a whole
// token, without regard to case.
func (s logtriageSubject) matches(text string) bool {
	return s.matchesLowered(strings.ToLower(text))
}

// matchesLowered is matches for a text already lowercased, so a text read once
// pays the lowercase once.
func (s logtriageSubject) matchesLowered(text string) bool {
	for _, w := range s.words {
		if s.matchWord(w, text) {
			return true
		}
	}
	return false
}

// isPageSeverity reports whether an escalation severity pages the operator. A
// critical or high escalation does; medium, low and an unset severity do not.
func isPageSeverity(severity string) bool {
	switch strings.ToLower(severity) {
	case "critical", "high":
		return true
	}
	return false
}
