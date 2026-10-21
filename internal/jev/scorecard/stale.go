package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/stale"
)

// StaleBattery is the stale battery's name, its --battery value.
const StaleBattery = "stale"

// statusClosed is the status of a bead that was closed.
const statusClosed = "closed"

// The rules that label a stale line, by the name --json reports.
const (
	ruleStaleClosedObsolete   = "stale:closed-obsolete"
	ruleStaleClosedSuperseded = "stale:closed-superseded"
	ruleStaleLanded           = "stale:landed"
	ruleStaleReshaped         = "stale:reshaped"
	ruleStalePriorityRaised   = "stale:priority-raised"
	ruleStalePriorityLowered  = "stale:priority-lowered"
)

// The question ids the stale battery logs, named by the stale package.
const (
	qObsolete     QuestionID = stale.QObsolete
	qSuperseded   QuestionID = stale.QSuperseded
	qWorthDoing   QuestionID = stale.QWorthDoing
	qNeedsReshape QuestionID = stale.QNeedsReshape
	qPriorityFit  QuestionID = stale.QPriorityFit
)

func init() {
	Register(StaleBattery, func(townRoot string) Adapter {
		return &staleAdapter{
			log:    stale.LogPath(townRoot),
			issues: &townNotes{town: townRoot, stores: map[string]*beads.Beads{}},
			lines:  map[string]stale.Line{},
			cache:  map[string]*beads.Issue{},
		}
	})
}

// staleBeadReader reads one bead by rig and id. townNotes is one.
type staleBeadReader interface {
	Show(rig, bead string) (*beads.Issue, error)
}

// staleAdapter labels each logged stale read from the bead as it stands at
// scorecard time. It reads and never writes.
type staleAdapter struct {
	log    string
	issues staleBeadReader
	// lines holds each parsed line by item ID, so Label has the line's rig,
	// bead and the priority the bead had when it was asked about.
	lines map[string]stale.Line
	// cache holds each bead read by rig and id, so lines that share a bead read
	// it once. A nil entry is a bead that does not exist.
	cache map[string]*beads.Issue
}

func (a *staleAdapter) Battery() string { return StaleBattery }
func (a *staleAdapter) LogPath() string { return a.log }

// staleID names one logged read: its bead and the moment it was logged, so a
// bead asked again after its 30 days is its own item.
func staleID(l stale.Line) string {
	return l.Rig + "/" + l.Bead + "@" + l.TS.UTC().Format(time.RFC3339Nano)
}

// Parse reads one stale line. A bead that could not be asked has no answers,
// one from an older question set cannot be compared with this one, and one about
// a rig, molecule or other non-work bead was logged before the pass read only
// work beads, so all three are skipped and counted.
func (a *staleAdapter) Parse(raw []byte) (Item, error) {
	var l stale.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Rig == "" || l.Bead == "" || l.TS.IsZero() {
		return Item{}, errors.New("stale line needs a rig, a bead and a ts")
	}
	if !stale.IsWorkType(l.Type) {
		return Item{}, fmt.Errorf("stale line is about a %q bead, the scorecard reads task, bug and feature", l.Type)
	}
	if l.QuestionVersion != stale.Version {
		return Item{}, fmt.Errorf("stale line asked question set %d, the scorecard reads %d", l.QuestionVersion, stale.Version)
	}
	if l.Answers == nil {
		return Item{}, fmt.Errorf("stale line has no answers: %s", l.Reason)
	}
	it := Item{
		ID:      staleID(l),
		Time:    l.TS,
		Model:   l.Model,
		Answers: make(map[QuestionID]Answer, len(l.Answers)),
	}
	for id, ans := range l.Answers {
		it.Answers[QuestionID(id)] = Answer{Noul: ans.Noul, Score: ans.Score}
	}
	a.lines[it.ID] = l
	return it, nil
}

// Label finds the labels for one logged read, from the bead's state now. A
// rule sets only the questions its meaning settles, and the rules that apply
// join their names with "+":
//
//  1. closed as wontfix, obsolete or no longer sets obsolete true;
//  2. closed as superseded or duplicate sets superseded true;
//  3. landed sets worth_doing true and obsolete and superseded false. A landed
//     bead's close reason is either the bare word "landed" or the landing
//     record's "Landed on ..." reason. If its description changed since the ask,
//     it also sets needs_reshaping true (stale:reshaped);
//  4. a priority changed since the ask sets priority_fit: raised is true (the
//     bead should rank higher), lowered is false (it should rank lower).
//
// A bead still open with its priority unchanged is unlabeled, and so is a bead
// that no longer exists. A line logged before the description hash existed is
// never labeled needs_reshaping, because it cannot show a change.
func (a *staleAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the stale adapter", item.ID)
	}
	issue, err := a.bead(line.Rig, line.Bead)
	if err != nil {
		return Labeling{}, err
	}
	if issue == nil {
		return Labeling{}, nil
	}

	labels := map[QuestionID]bool{}
	var rules []string
	if strings.TrimSpace(issue.Status) == statusClosed {
		reason := strings.TrimSpace(issue.CloseReason)
		switch {
		case stale.IsLandedReason(reason):
			labels[qWorthDoing] = true
			labels[qObsolete] = false
			labels[qSuperseded] = false
			rules = append(rules, ruleStaleLanded)
			if reshaped(line, issue) {
				labels[qNeedsReshape] = true
				rules = append(rules, ruleStaleReshaped)
			}
		case hasAnyPrefix(strings.ToLower(reason), "wontfix", "obsolete", "no longer"):
			labels[qObsolete] = true
			rules = append(rules, ruleStaleClosedObsolete)
		case hasAnyPrefix(strings.ToLower(reason), "superseded", "duplicate"):
			labels[qSuperseded] = true
			rules = append(rules, ruleStaleClosedSuperseded)
		}
	}
	// A lower P number is more urgent, so a smaller priority now is a raise.
	switch {
	case issue.Priority < line.Priority:
		labels[qPriorityFit] = true
		rules = append(rules, ruleStalePriorityRaised)
	case issue.Priority > line.Priority:
		labels[qPriorityFit] = false
		rules = append(rules, ruleStalePriorityLowered)
	}
	if len(rules) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// bead reads the bead a line names, once per rig and id. A bead that no longer
// exists reads as nil; any other read error fails the report.
func (a *staleAdapter) bead(rig, id string) (*beads.Issue, error) {
	key := rig + "/" + id
	if issue, ok := a.cache[key]; ok {
		return issue, nil
	}
	issue, err := a.issues.Show(rig, id)
	if errors.Is(err, beads.ErrNotFound) {
		issue, err = nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading bead %s in %s: %w", id, rig, err)
	}
	a.cache[key] = issue
	return issue, nil
}

// reshaped reports whether a bead's description changed after the line asked
// about it. A line that carries no hash was logged before the field existed,
// and a bead with no description now gives no text to compare — a read that
// dropped the description reads the same as one that removed it — so neither
// shows a change. A bead asked about bare and described since does, because
// its line records the hash of the empty text.
func reshaped(line stale.Line, issue *beads.Issue) bool {
	if line.DescHash == "" || strings.TrimSpace(issue.Description) == "" {
		return false
	}
	return stale.HashDescription(issue.Description) != line.DescHash
}

// hasAnyPrefix reports whether s starts with any of the prefixes.
func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
