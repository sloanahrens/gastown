package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
	// ruleProgressScope is a proxy for drift, not ground truth: a refusal that
	// names scope is evidence the work left the bead, and nothing cheaper is
	// recorded (gt-tpv79).
	ruleProgressScope = "progress:rejection-names-scope"
)

// The question ids the progress battery logs, named by the progress package.
const (
	qMakingProgress       QuestionID = progress.QMakingProgress
	qBlockedOnEnvironment QuestionID = progress.QBlockedOnEnvironment
	qRemaining            QuestionID = progress.QRemaining
	qLooping              QuestionID = progress.QLooping
	qDriftedFromBead      QuestionID = progress.QDriftedFromBead
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
	// A score's meaning comes from its level order, so a line from another
	// question set would be swept with its direction reversed.
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
//     sets making_progress true, remaining true (done or nearly done), and
//     looping and drifted_from_bead false: a bead that landed was neither
//     repeating a command nor working away from its bead;
//  2. no landing in the window sets remaining false: the bead was not shown
//     done or nearly done at the nudge. A bead the store no longer holds takes
//     no remaining label, since its state cannot be read;
//  3. else an overrun stop at or after the nudge sets making_progress false,
//     and looping true unless the stop was charged to gate time: a stop the
//     environment caused says nothing about the seat repeating itself. A bead
//     the store no longer holds takes no looping label either;
//  4. an overrun record whose gate time was charged at or after the nudge sets
//     blocked_on_environment true;
//  5. a MERGE REJECTION block written after the nudge whose own words name
//     scope drift sets drifted_from_bead true.
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
		rules = append(rules, ruleProgressLanded)
		labels[qMakingProgress] = true
		labels[qRemaining] = true
		labels[qLooping] = false
		labels[qDriftedFromBead] = false
	} else {
		if !missing {
			labels[qRemaining] = false
			rules = append(rules, ruleProgressNotLanded)
		}
		if !stop.StoppedAt.IsZero() && !stop.StoppedAt.Before(nudged) {
			labels[qMakingProgress] = false
			rules = append(rules, ruleProgressStopped)
			// A bead the store no longer holds takes no looping label: the
			// record cannot be read against the bead, so the stop is not
			// charged to the seat repeating itself.
			if !missing && !gateCharged(stop, nudged) {
				labels[qLooping] = true
			}
		}
		if rejectionNamesScope(notes, nudged) {
			labels[qDriftedFromBead] = true
			rules = append(rules, ruleProgressScope)
		}
	}
	if gateCharged(stop, nudged) {
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

// gateCharged reports whether a stop was charged to gate time seen at or after
// nudged. Such a stop is the environment's wait rather than the seat repeating
// itself, so it leaves looping unlabeled.
func gateCharged(stop progressStop, nudged time.Time) bool {
	return stop.GateSpent > 0 && !stop.GateSeenAt.Before(nudged)
}

// scopeDriftRE matches a word a refusal names scope drift with, whole and in
// either case. The whole-word form keeps telescoped from reading as a drift.
var scopeDriftRE = regexp.MustCompile(`(?i)\b(scope|unrelated|beyond)\b`)

// rejectionNamesScope reports whether notes hold a MERGE REJECTION block
// written after nudged whose own words name scope drift.
func rejectionNamesScope(notes string, nudged time.Time) bool {
	for _, rej := range progressRejections(notes) {
		// A refusal is placed by the submission it answers. One answering a
		// submission made before the nudge has no time of its own to place,
		// so it stays unlabeled: a refusal that may predate the nudge is not
		// evidence about the seat's work after it.
		if rej.submitted.IsZero() || rej.submitted.Before(nudged) {
			continue
		}
		if scopeDriftRE.MatchString(rej.words) {
			return true
		}
	}
	return false
}

// progressRejection is one MERGE REJECTION block in a bead's notes, the
// submission it answers, and the words the refusal itself wrote.
type progressRejection struct {
	// submitted is the Submitted stamp of the READY TO LAND block the refusal
	// answers, the last one the notes hold before it. Zero when that block
	// carries no readable stamp.
	submitted time.Time
	// words is the refusal's reason and each finding title, the text it
	// argued in. The block's structural fields and its quoted gate tail are
	// left out, so a branch name or a file path in output the refusal quoted
	// never reads as drift (gt-tpv79).
	words string
}

// rejectionWords reads the words a refusal wrote, through land's own parser:
// its reason, then each finding's title.
func rejectionWords(block string) string {
	rej, ok := land.ParseRejectionNote(block)
	if !ok {
		return ""
	}
	words := make([]string, 0, len(rej.Findings)+1)
	words = append(words, rej.Reason)
	for _, f := range rej.Findings {
		words = append(words, f.Title)
	}
	return strings.Join(words, "\n")
}

// progressRejections returns the notes' MERGE REJECTION blocks in the order
// they were written. The blocks carry no time of their own, so a block is
// placed by the submission it answers: bd appends notes in order, so the READY
// TO LAND block before it is that submission, and the refusal was written after
// it.
func progressRejections(notes string) []progressRejection {
	lines := strings.Split(notes, "\n")
	var out []progressRejection
	var submitted time.Time
	for i := 0; i < len(lines); i++ {
		switch {
		case lines[i] == land.ReadyNoteMarker:
			submitted = readySubmitted(lines[i+1:])
		case isRejectionHeader(lines[i]):
			start := i
			for i+1 < len(lines) && !opensNoteBlock(lines[i+1]) {
				i++
			}
			block := strings.Join(lines[start:i+1], "\n")
			out = append(out, progressRejection{submitted: submitted, words: rejectionWords(block)})
		}
	}
	return out
}

// opensNoteBlock reports whether line starts a note block the progress adapter
// reads, so a rejection's text stops there: a later round's submission or
// refusal never lends this one its words.
func opensNoteBlock(line string) bool {
	return line == land.ReadyNoteMarker || line == land.LandingNoteMarker || isRejectionHeader(line)
}

// isRejectionHeader reports whether line opens a MERGE REJECTION block. It
// reads the header through land's own parser, so a marker quoted inside a
// refusal's reason, finding or gate tail is content and never a block
// (gt-9bioi.1).
func isRejectionHeader(line string) bool {
	if !strings.HasPrefix(line, land.MergeRejectionNoteMarker+" ") {
		return false
	}
	_, ok := land.ParseRejectionNote(line)
	return ok
}

// readySubmitted reads a READY TO LAND block's Submitted stamp, with the field
// rules land.ParseReadyNote reads: the fields run to the first line that is not
// one, and a stamp no clock can read orders as none, rather than failing the
// whole report over a bead whose work is fine.
func readySubmitted(fields []string) time.Time {
	for _, line := range fields {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return time.Time{}
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "branch", "head", "target", "worker":
		case "submitted":
			at, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
			if err != nil {
				return time.Time{}
			}
			return at
		default:
			return time.Time{}
		}
	}
	return time.Time{}
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
