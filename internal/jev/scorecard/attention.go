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
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// AttentionBattery is the attention battery's name, its --battery value.
const AttentionBattery = "attention"

// selfClearWindow is how soon after it was raised an item that cleared with no
// ack, note or close reason is taken to have cleared on its own.
const selfClearWindow = 15 * time.Minute

// The rules that label an attention item, by the name --json reports.
const (
	ruleAckNeededHuman        = "ack:needed-human"
	ruleAckNoise              = "ack:noise"
	ruleAckFixedByOverseer    = "ack:fixed-by-overseer"
	ruleEscalationByPerson    = "escalation:closed-by-person"
	ruleEscalationByProducer  = "escalation:cleared-by-producer"
	ruleRiskPathFail          = "risk-path:review-fail"
	ruleRiskPathPass          = "risk-path:review-pass"
	ruleRejectedSuperseded    = "rejected-twice:superseded"
	ruleRejectedLandedRoutine = "rejected-twice:landed-routine"
	ruleClearedWithinWindow   = "cleared-within-15m"
)

// beadShower reads one bead by id. *beads.Beads is one.
type beadShower interface {
	Show(id string) (*beads.Issue, error)
}

func init() {
	Register(AttentionBattery, func(townRoot string) Adapter {
		return &attentionAdapter{town: townRoot, beads: beads.New(townRoot), lines: map[string]jevattention.Line{}}
	})
}

// attentionAdapter scores the attention battery's log against the attention
// queue, its acks history and the beads the items name. It reads and never
// writes; Label reads the retained queue files once, on its first call.
type attentionAdapter struct {
	town  string
	beads beadShower
	// lines holds each parsed line by item ID, so Label has the line's key,
	// kind, bead and commit. Build parses every line before it labels any.
	lines map[string]jevattention.Line
	// beadCache holds each bead read by id, so items that share a bead read it
	// once. A nil entry is a bead that does not exist.
	beadCache map[string]*beads.Issue

	loaded  bool
	records map[string][]attention.Record
	history []attention.AckRecord
}

func (a *attentionAdapter) Battery() string { return AttentionBattery }
func (a *attentionAdapter) LogPath() string { return jevattention.LogPath(a.town) }

// attentionID names one logged item: its key and the first_seen that tells
// its life apart from a recurrence of the same key.
func attentionID(l jevattention.Line) string {
	return l.Key + "@" + l.FirstSeen.UTC().Format(time.RFC3339Nano)
}

// Parse reads one battery line. A line whose call failed has no answers and
// one from an older question set cannot be compared with this one, so both
// are skipped and counted.
func (a *attentionAdapter) Parse(raw []byte) (Item, error) {
	var l jevattention.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Key == "" || l.FirstSeen.IsZero() {
		return Item{}, errors.New("attention line needs a key and a first_seen")
	}
	if l.QuestionVersion != jevattention.QuestionVersion {
		return Item{}, fmt.Errorf("attention line asked question set %d, the scorecard reads %d", l.QuestionVersion, jevattention.QuestionVersion)
	}
	if l.Answers == nil {
		return Item{}, errors.New("attention line has no answers: its call failed")
	}
	ans := l.Answers
	it := Item{
		ID:    attentionID(l),
		Time:  l.TS,
		Model: l.Model,
		Answers: map[QuestionID]Answer{
			jevattention.QNeedsHuman:         {Noul: ptr(ans.NeedsHuman)},
			jevattention.QClearsOnItsOwn:     {Noul: ptr(ans.ClearsOnItsOwn)},
			jevattention.QRoutineActionFixes: {Noul: ptr(ans.RoutineActionFixes)},
			jevattention.QRepeatsOpenItem:    {Noul: ptr(ans.RepeatsOpenItem)},
			jevattention.QUrgency:            {Score: ptr(ans.Urgency.Score)},
		},
	}
	a.lines[it.ID] = l
	return it, nil
}

// Label finds the labels for one logged item, in order: a disposition a human
// gave it in the acks history, which wins over everything else; else the
// clearing mechanism of its kind; else the self-clear window. An item still
// standing with no disposition is unlabeled.
func (a *attentionAdapter) Label(ctx context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the attention adapter", item.ID)
	}
	if err := a.load(); err != nil {
		return Labeling{}, err
	}
	rec, ok := lifeOf(a.records[line.Key], line.FirstSeen)
	if !ok {
		// The retained queue files no longer hold this life, so nothing says
		// how it ended.
		return Labeling{}, nil
	}

	if d, ok := a.disposition(rec); ok {
		return dispositionLabeling(d), nil
	}
	if rec.ClearedAt == nil {
		return Labeling{}, nil
	}

	// The bead and commit come from the battery line: the queue keeps them only
	// while an item stands, so a cleared life has none in the queue files.
	issue, err := a.bead(line.Bead)
	if err != nil {
		return Labeling{}, err
	}
	if lab, ok := mechanismLabeling(rec.Kind, line.SHA, issue); ok {
		return lab, nil
	}
	if rec.ClearedAt.Sub(rec.FirstSeen) <= selfClearWindow && !a.acked(rec) && quiet(line, issue) {
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QClearsOnItsOwn: true}, Rule: ruleClearedWithinWindow}, nil
	}
	return Labeling{}, nil
}

// load reads the queue and the acks history once.
func (a *attentionAdapter) load() error {
	if a.loaded {
		return nil
	}
	snap, err := attention.Read(a.town)
	if err != nil {
		return fmt.Errorf("reading the attention queue: %w", err)
	}
	history, err := attention.ReadAcksHistory(a.town)
	if err != nil {
		return fmt.Errorf("reading acks-history.jsonl: %w", err)
	}
	a.records = map[string][]attention.Record{}
	for _, r := range snap.Items {
		a.records[r.Key] = append(a.records[r.Key], r)
	}
	a.history = history
	a.loaded = true
	return nil
}

// Prefetch reads every bead the parsed lines name in one batch, so bead finds
// them cached. A bead the batch did not return is read one at a time.
func (a *attentionAdapter) Prefetch(_ context.Context, items []Item) {
	var ids []string
	for _, it := range items {
		ids = append(ids, a.lines[it.ID].Bead)
	}
	for id, issue := range showAll(a.beads, ids) {
		if a.beadCache == nil {
			a.beadCache = map[string]*beads.Issue{}
		}
		a.beadCache[id] = issue
	}
}

// bead reads the bead an item names, once per id. A bead that no longer exists
// reads as nil, so a label that depends on it is not made; any other read error
// fails the report.
func (a *attentionAdapter) bead(id string) (*beads.Issue, error) {
	if id == "" {
		return nil, nil
	}
	if issue, ok := a.beadCache[id]; ok {
		return issue, nil
	}
	issue, err := a.beads.Show(id)
	if errors.Is(err, beads.ErrNotFound) {
		issue, err = nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading bead %s: %w", id, err)
	}
	if a.beadCache == nil {
		a.beadCache = map[string]*beads.Issue{}
	}
	a.beadCache[id] = issue
	return issue, nil
}

// disposition is the latest disposition a human gave this life, from acks made
// while it stood. A line written before dispositions existed has none, and
// does not count as one.
func (a *attentionAdapter) disposition(rec attention.Record) (attention.Disposition, bool) {
	var (
		d     attention.Disposition
		found bool
		at    time.Time
	)
	for _, h := range a.history {
		if h.Key != rec.Key || h.Disposition == "" || !inLife(h.TS, rec) {
			continue
		}
		if !found || !h.TS.Before(at) {
			d, at, found = h.Disposition, h.TS, true
		}
	}
	return d, found
}

// acked reports whether anyone acked this life, with or without a disposition.
func (a *attentionAdapter) acked(rec attention.Record) bool {
	for _, h := range a.history {
		if h.Key == rec.Key && inLife(h.TS, rec) {
			return true
		}
	}
	return false
}

// inLife reports whether at falls while rec stood: from its raise to its clear,
// or to now while it still stands.
func inLife(at time.Time, rec attention.Record) bool {
	if at.Before(rec.FirstSeen) {
		return false
	}
	return rec.ClearedAt == nil || !at.After(*rec.ClearedAt)
}

// lifeOf returns the record of the life raised at first.
func lifeOf(recs []attention.Record, first time.Time) (attention.Record, bool) {
	for _, r := range recs {
		if r.FirstSeen.Equal(first) {
			return r, true
		}
	}
	return attention.Record{}, false
}

// dispositionLabeling is the label a disposition sets. Each disposition sets
// only the questions its meaning settles.
func dispositionLabeling(d attention.Disposition) Labeling {
	switch d {
	case attention.DispositionNeededHuman:
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QNeedsHuman: true}, Rule: ruleAckNeededHuman}
	case attention.DispositionNoise:
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{
			jevattention.QNeedsHuman:     false,
			jevattention.QClearsOnItsOwn: true,
		}, Rule: ruleAckNoise}
	case attention.DispositionFixedByOverseer:
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QRoutineActionFixes: true}, Rule: ruleAckFixedByOverseer}
	}
	return Labeling{}
}

// mechanismLabeling is the label the kind's clearing mechanism sets, for an
// item that cleared. sha is the commit the item names. ok is false when the
// mechanism says nothing.
func mechanismLabeling(kind attention.Kind, sha string, issue *beads.Issue) (Labeling, bool) {
	if issue == nil {
		return Labeling{}, false
	}
	switch kind {
	case attention.KindEscalation:
		if strings.TrimSpace(issue.CloseReason) == "" {
			return Labeling{}, false
		}
		// A close by the person or the overseer decides the escalation. A
		// producer's clear closes it with a reason too, so the closer tells
		// them apart.
		if attention.IsHumanAddress(beads.ParseEscalationFields(issue.Description).ClosedBy) {
			return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QNeedsHuman: true}, Rule: ruleEscalationByPerson}, true
		}
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QClearsOnItsOwn: true}, Rule: ruleEscalationByProducer}, true

	case attention.KindRiskPath:
		verdict, ok := land.OverseerReviewVerdict(issue.Notes, sha)
		switch {
		case ok && verdict == land.OverseerReviewFail:
			return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QNeedsHuman: true}, Rule: ruleRiskPathFail}, true
		case ok && verdict == land.OverseerReviewPass:
			return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QNeedsHuman: false}, Rule: ruleRiskPathPass}, true
		}
		return Labeling{}, false

	case attention.KindRejectedTwice:
		reason := strings.TrimSpace(issue.CloseReason)
		if hasReasonPrefix(reason, "superseded") {
			return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QNeedsHuman: true}, Rule: ruleRejectedSuperseded}, true
		}
		if hasReasonPrefix(reason, "Landed on") {
			// A landing from a crew branch is a person's rework; one from a
			// polecat branch is the routine route fixing it.
			branch := landedBranch(issue.Notes)
			if branch != "" && !strings.HasPrefix(branch, "crew/") {
				return Labeling{Labeled: true, Labels: map[QuestionID]bool{jevattention.QRoutineActionFixes: true}, Rule: ruleRejectedLandedRoutine}, true
			}
		}
	}
	return Labeling{}, false
}

// landedBranch is the branch of the last LANDING RECORD block in notes, or "".
func landedBranch(notes string) string {
	branch := ""
	inRecord := false
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		if line == land.LandingNoteMarker {
			inRecord, branch = true, ""
			continue
		}
		if !inRecord {
			continue
		}
		if v, ok := strings.CutPrefix(line, "branch: "); ok {
			branch = strings.TrimSpace(v)
		}
	}
	return branch
}

// hasReasonPrefix reports whether a close reason starts with prefix, ignoring
// case: a person writes "Superseded by ..." as readily as "superseded by ...",
// and "Landed on" is matched the same way so neither prefix is the stricter.
func hasReasonPrefix(reason, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(reason), strings.ToLower(prefix))
}

// quiet reports whether nothing shows that a person or the overseer dealt with
// the item: its bead carries no note and no close reason, the two things they
// leave. That needs a bead to read. A line that names none is quiet only when
// the item was open when the pass asked, so it truly had none; a line written
// after the item cleared carries no bead whatever the item had. A bead the line
// names but that no longer exists says nothing either, so neither is quiet.
func quiet(line jevattention.Line, issue *beads.Issue) bool {
	if line.Bead == "" {
		return line.Live
	}
	return issue != nil && strings.TrimSpace(issue.Notes) == "" && strings.TrimSpace(issue.CloseReason) == ""
}
