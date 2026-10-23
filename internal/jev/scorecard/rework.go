package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/jev/rework"
	"github.com/steveyegge/gastown/internal/land"
)

// ReworkBattery is the refused-review cause battery's name, its --battery value.
const ReworkBattery = "rework"

// The rules that label a refusal line, by the name --json reports.
const (
	ruleReworkSameHead = "rework:same-head-landed"
	ruleReworkNewHead  = "rework:reworked-before-landing"
)

// qReworkedToLand is the question id the rework battery logs its
// reworked_to_land noul under, named by the rework package.
const qReworkedToLand QuestionID = rework.QReworkedToLand

func init() {
	Register(ReworkBattery, func(townRoot string) Adapter {
		return newReworkAdapter(rework.LogPath(townRoot), townRoot)
	})
}

// reworkAdapter labels each refusal line's reworked_to_land from the rig's
// landings file. It reads and never writes. Each rig's landings are read once,
// on the first line that names it.
type reworkAdapter struct {
	log      string
	townRoot string

	// lines holds each parsed line by item ID, so Label has the bead, the head
	// the refusal judged and when.
	lines map[string]rework.Line
	// landings holds each rig's records, read once.
	landings map[string]rigLandings
}

// rigLandings is one rig's landings file as it stood on the first read.
type rigLandings struct {
	recs []land.LandingRecord
	err  error
}

func newReworkAdapter(log, townRoot string) *reworkAdapter {
	return &reworkAdapter{log: log, townRoot: townRoot,
		lines: map[string]rework.Line{}, landings: map[string]rigLandings{}}
}

func (a *reworkAdapter) Battery() string { return ReworkBattery }
func (a *reworkAdapter) LogPath() string { return a.log }

// reworkID names one judged refusal: its rig, its bead and the attempt it
// belongs to, so a bead refused twice is its own item twice.
func reworkID(l rework.Line) string {
	return l.Rig + "/" + l.Bead + "#" + strconv.Itoa(l.Attempt)
}

// Parse reads one refusal line. A line that has no answers, names no
// submission the refusal judged, or was asked under a question set newer than
// this scorecard knows cannot be compared with this one, so it is skipped.
func (a *reworkAdapter) Parse(raw []byte) (Item, error) {
	var l rework.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Rig == "" || l.Bead == "" || l.Attempt < 1 || l.TS.IsZero() || l.Refused.IsZero() {
		return Item{}, errors.New("rework line needs a rig, a bead, an attempt, a ts and a refused time")
	}
	if !readableQuestionSet(l.QuestionVersion, rework.Version) {
		return Item{}, fmt.Errorf("rework line asked question set %d, the scorecard reads %d or older", l.QuestionVersion, rework.Version)
	}
	if l.Answers == nil {
		return Item{}, errors.New("rework line has no answers")
	}
	it := Item{
		ID:      reworkID(l),
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

// Label sets reworked_to_land for one judged refusal from the rig's landings.
// The label is the first landing of the bead after the refusal: the refused
// head itself landing is false, since the refusal let the work through without
// a rework commit, and any other head landing is true, since the bead landed
// only after a later submission. A line asked after reworked_to_land left the
// battery is unlabeled: it gave no answer for the label to score against, even
// though the landings could label it. A bead that has not landed yet is
// unlabeled: the answer is still open. So is a refusal whose rejection block
// named no head, since no landing can be compared with it. So is a first
// landing that names no head: it cannot be told from the refused head, so it
// is not labeled as a later one.
func (a *reworkAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the rework adapter", item.ID)
	}
	if _, asked := line.Answers[rework.QReworkedToLand]; !asked {
		return Labeling{}, nil
	}
	if line.Head == "" {
		return Labeling{}, nil
	}
	recs, err := a.landingsFor(line.Rig)
	if err != nil {
		return Labeling{}, err
	}
	first, ok := firstLandingAfter(recs, line)
	if !ok || first.Head == "" {
		return Labeling{}, nil
	}
	if first.Head == line.Head {
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{qReworkedToLand: false}, Rule: ruleReworkSameHead}, nil
	}
	return Labeling{Labeled: true, Labels: map[QuestionID]bool{qReworkedToLand: true}, Rule: ruleReworkNewHead}, nil
}

// landingsFor reads the rig's landings file once.
func (a *reworkAdapter) landingsFor(rig string) ([]land.LandingRecord, error) {
	if got, ok := a.landings[rig]; ok {
		return got.recs, got.err
	}
	var got rigLandings
	file, err := land.RigLandingsFile(a.townRoot, rig)
	if err != nil {
		got.err = fmt.Errorf("reading rig %s's landings file: %w", rig, err)
	} else if recs, err := file.Since(time.Time{}); err != nil {
		got.err = fmt.Errorf("reading rig %s's landings: %w", rig, err)
	} else {
		got.recs = recs
	}
	a.landings[rig] = got
	return got.recs, got.err
}

// firstLandingAfter returns the bead's earliest landing record after the
// refusal, which is the landing the refusal was or was not reworked into.
func firstLandingAfter(recs []land.LandingRecord, l rework.Line) (land.LandingRecord, bool) {
	var first land.LandingRecord
	found := false
	for _, r := range recs {
		if r.BeadID != l.Bead || r.LandedAt.IsZero() || !r.LandedAt.After(l.Refused) {
			continue
		}
		if !found || r.LandedAt.Before(first.LandedAt) {
			first, found = r, true
		}
	}
	return first, found
}
