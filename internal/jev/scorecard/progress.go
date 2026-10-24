package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/progress"
	"github.com/steveyegge/gastown/internal/land"
)

// ProgressBattery is the progress battery's name, its --battery value.
const ProgressBattery = "progress"

// progressWindow is how long after the nudge a landing counts as the polecat
// making progress.
const progressWindow = 20 * time.Minute

// progressSettle is how old a line must be before it is labeled. A younger
// line stays unlabeled, since its bead may still be moving.
const progressSettle = 30 * time.Minute

// The rules that label a progress line, by the name --json reports.
const (
	ruleProgressLanded    = "progress:landed-in-window"
	ruleProgressNotLanded = "progress:not-landed-in-window"
	ruleProgressStopped   = "progress:stopped-by-overrun"
	ruleProgressGateWait  = "progress:gate-wait"
)

// The question ids the progress battery logs, named by the progress package.
const (
	qMakingProgress       QuestionID = progress.QMakingProgress
	qBlockedOnEnvironment QuestionID = progress.QBlockedOnEnvironment
	qRemaining            QuestionID = progress.QRemaining
)

func init() {
	Register(ProgressBattery, func(townRoot string) Adapter {
		return &progressAdapter{
			town:  townRoot,
			notes: &townNotes{town: townRoot, stores: map[string]*beads.Beads{}},
			lines: map[string]progress.Line{},
			now:   time.Now,
		}
	})
}

// progressAdapter labels each logged nudge from the bead the nudge named: the
// landing record it gained after the nudge, and the overrun guard's record for
// the bead. It reads and never writes.
type progressAdapter struct {
	town  string
	notes landingNotes
	// lines holds each parsed line by item ID, so Label has the line's bead
	// and nudge time.
	lines map[string]progress.Line
	now   func() time.Time
}

func (a *progressAdapter) Battery() string { return ProgressBattery }
func (a *progressAdapter) LogPath() string { return progress.LogPath(a.town) }

// progressID names one logged nudge: its bead and the moment it was delivered,
// so a second nudge of the same bead in a later sling is its own item.
func progressID(l progress.Line) string {
	return l.Bead + "@" + l.NudgeAt.UTC().Format(time.RFC3339Nano)
}

// Parse reads one progress line. A nudge that was not asked has no answers,
// and one from an older question set cannot be compared with this one, so both
// are skipped and counted.
func (a *progressAdapter) Parse(raw []byte) (Item, error) {
	var l progress.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Rig == "" || l.Bead == "" || strings.ContainsAny(l.Bead, `/\`) || l.TS.IsZero() || l.NudgeAt.IsZero() {
		return Item{}, errors.New("progress line needs a rig, a bead, a ts and a nudge_at")
	}
	if l.QuestionVersion != progress.Version {
		return Item{}, fmt.Errorf("progress line asked question set %d, the scorecard reads %d", l.QuestionVersion, progress.Version)
	}
	if l.Answers == nil {
		return Item{}, fmt.Errorf("progress line has no answers: %s", l.Reason)
	}
	it := Item{
		ID:      progressID(l),
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

// Label finds the labels for one logged nudge. A line younger than the settle
// window is unlabeled. Otherwise the rules are, in order:
//
//  1. a landing record that names a landing within the window after the nudge
//     sets making_progress true and remaining true (done or nearly done);
//  2. no landing in the window sets remaining false: the bead was not shown
//     done or nearly done at the nudge. A bead the store no longer holds takes
//     no remaining label, since its state cannot be read;
//  3. else an overrun stop at or after the nudge sets making_progress false;
//  4. an overrun record whose gate time was charged at or after the nudge sets
//     blocked_on_environment true.
//
// Rules 1 and 3 are exclusive: a landing in the window is progress, so a stop
// cannot also be the answer for the same nudge. The rule that set each label is
// joined into the rule name with "+".
func (a *progressAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the progress adapter", item.ID)
	}
	if a.now().Sub(item.Time) < progressSettle {
		return Labeling{}, nil
	}
	notes, err := a.notes.Notes(line.Rig, line.Bead)
	missing := errors.Is(err, beads.ErrNotFound)
	if missing {
		notes, err = "", nil
	}
	if err != nil {
		return Labeling{}, fmt.Errorf("reading the notes of %s: %w", line.Bead, err)
	}
	stop, err := readOverrunStop(a.town, line.Bead)
	if err != nil {
		return Labeling{}, err
	}

	nudged := line.NudgeAt
	labels := map[QuestionID]bool{}
	var rules []string
	if rec, landed := land.ParseLandingRecord(notes); landed && inProgressWindow(rec.LandedAt, nudged) {
		labels[qMakingProgress] = true
		labels[qRemaining] = true
		rules = append(rules, ruleProgressLanded)
	} else {
		if !missing {
			labels[qRemaining] = false
			rules = append(rules, ruleProgressNotLanded)
		}
		if !stop.StoppedAt.IsZero() && !stop.StoppedAt.Before(nudged) {
			labels[qMakingProgress] = false
			rules = append(rules, ruleProgressStopped)
		}
	}
	if stop.GateSpent > 0 && !stop.GateSeenAt.Before(nudged) {
		labels[qBlockedOnEnvironment] = true
		rules = append(rules, ruleProgressGateWait)
	}
	if len(labels) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: strings.Join(rules, "+")}, nil
}

// inProgressWindow reports whether landed falls from the nudge to the window's
// end.
func inProgressWindow(landed, nudged time.Time) bool {
	return !landed.Before(nudged) && landed.Sub(nudged) <= progressWindow
}

// progressStop is the part of the overrun guard's per-bead record the progress
// battery reads: when the guard stopped the seat, and the gate time it charged
// to the sling.
type progressStop struct {
	StoppedAt  time.Time     `json:"stopped_at"`
	GateSpent  time.Duration `json:"gate_spent"`
	GateSeenAt time.Time     `json:"gate_seen_at"`
}

// readOverrunStop reads the overrun guard's record for bead. A missing record
// is the zero stop; a record that cannot be read is an error.
func readOverrunStop(town, bead string) (progressStop, error) {
	path := filepath.Join(constants.TownRuntimePath(town), "overrun-guard", bead+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return progressStop{}, nil
	}
	if err != nil {
		return progressStop{}, fmt.Errorf("reading the overrun record for %s: %w", bead, err)
	}
	var st progressStop
	if err := json.Unmarshal(data, &st); err != nil {
		return progressStop{}, fmt.Errorf("parsing the overrun record for %s: %w", bead, err)
	}
	return st, nil
}
