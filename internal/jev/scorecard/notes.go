package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/jev/notes"
)

// NotesBattery is the notes battery's name, its --battery value.
const NotesBattery = "notes"

// The rules that label a notes line, by the name --json reports.
const (
	ruleNotesStale   = "notes:stale-token"
	ruleNotesCurrent = "notes:all-tokens-resolve"
)

// The question ids the notes battery logs, named by the notes package.
const (
	qStillDescribes QuestionID = notes.QStillDescribes
	qNamesRemoved   QuestionID = notes.QNamesRemoved
	qNotesKind      QuestionID = notes.QKind
)

func init() {
	Register(NotesBattery, func(townRoot string) Adapter {
		return &notesAdapter{log: notes.LogPath(townRoot), lines: map[string]notes.Line{}}
	})
}

// notesAdapter labels each logged read from the mechanical label the pass wrote
// on the same line, so it reads the log and nothing else.
type notesAdapter struct {
	log string
	// lines holds each parsed line by item ID, so Label has the label and the
	// tokens it rests on.
	lines map[string]notes.Line
}

func (a *notesAdapter) Battery() string { return NotesBattery }
func (a *notesAdapter) LogPath() string { return a.log }

// notesID names one logged read: its file and the moment it was logged, so a
// file asked again is its own item.
func notesID(l notes.Line) string {
	return l.Source + "/" + l.File + "@" + l.TS.UTC().Format(time.RFC3339Nano)
}

// Parse reads one notes line. A file that could not be asked has no answers,
// and one from another question set cannot be compared with this one, so both
// are skipped and counted. A choice answer is scored by its probability for the
// option Jev picked, so the picked option is the positive one.
func (a *notesAdapter) Parse(raw []byte) (Item, error) {
	var l notes.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Source == "" || l.File == "" || l.TS.IsZero() {
		return Item{}, errors.New("notes line needs a source, a file and a ts")
	}
	if l.QuestionVersion != notes.Version {
		return Item{}, fmt.Errorf("notes line asked question set %d, the scorecard reads %d", l.QuestionVersion, notes.Version)
	}
	if l.Answers == nil {
		return Item{}, fmt.Errorf("notes line has no answers: %s", l.Reason)
	}
	it := Item{
		ID:      notesID(l),
		Time:    l.TS,
		Model:   l.Model,
		Answers: make(map[QuestionID]Answer, len(l.Answers)),
	}
	for id, ans := range l.Answers {
		out := Answer{Noul: ans.Noul}
		if ans.Choice != "" {
			out.Choice, out.Probabilities, out.Positive = ans.Choice, ans.Probabilities, ans.Choice
		}
		it.Answers[QuestionID(id)] = out
	}
	a.lines[it.ID] = l
	return it, nil
}

// Label finds the labels for one logged read from its mechanical label:
//
//  1. stale (a path missing from main, or a subcommand or flag absent from its
//     binary's --help) sets still_describes_the_town false and
//     names_something_removed true. kind is true when the option Jev picked is
//     the kind of one of the missing tokens;
//  2. current (every token resolved) sets still_describes_the_town true,
//     names_something_removed false, and kind true only when Jev picked none.
//
// A file with no tokens, or whose tokens cannot all be checked, is unlabeled.
func (a *notesAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	line, ok := a.lines[item.ID]
	if !ok {
		return Labeling{}, fmt.Errorf("item %s was not parsed by the notes adapter", item.ID)
	}
	picked := item.Answers[qNotesKind].Choice
	switch line.Label {
	case notes.LabelStale:
		labels := map[QuestionID]bool{qStillDescribes: false, qNamesRemoved: true}
		if picked != "" {
			labels[qNotesKind] = kindsOf(line.Missing)[picked]
		}
		return Labeling{Labeled: true, Labels: labels, Rule: ruleNotesStale}, nil
	case notes.LabelCurrent:
		labels := map[QuestionID]bool{qStillDescribes: true, qNamesRemoved: false}
		if picked != "" {
			labels[qNotesKind] = picked == notes.KindNone
		}
		return Labeling{Labeled: true, Labels: labels, Rule: ruleNotesCurrent}, nil
	}
	return Labeling{}, nil
}

// kindsOf is the set of token kinds among missing.
func kindsOf(missing []notes.Token) map[string]bool {
	out := make(map[string]bool, len(missing))
	for _, m := range missing {
		out[m.Kind] = true
	}
	return out
}
