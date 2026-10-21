package notes

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Reasons a line carries instead of answers. They are fixed words, never an
// error message, so the log never holds a path or a note's text.
const (
	ReasonBadResponse = "bad_response"
	ReasonAskFailed   = "ask_failed"
)

// ErrBusy means another pass holds the run lock.
var ErrBusy = errors.New("another notes pass holds the run lock")

// Answer is Jev's answer to one question. A noul sets Noul; the choice sets
// Choice and Probabilities.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Choice is the option with the highest probability.
	Choice string `json:"choice,omitempty"`
	// Probabilities maps every option to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Line is one line of the notes log: what Jev answered about one file, and the
// mechanical label the file earned the same pass. It holds the file's name, a
// hash of its content and the tokens the label rests on, never the text. A file
// that could not be asked has null answers and a Reason.
type Line struct {
	// TS is when the line was logged. A file is not asked again until AskAgain
	// has passed since this time, unless its Hash changes.
	TS time.Time `json:"ts"`
	// Source is the family the file belongs to, SourceMemory or SourceHandbook,
	// and File its name within it.
	Source string `json:"source"`
	File   string `json:"file"`
	// Hash is HashContent of the file's text when it was asked about.
	Hash string `json:"hash"`
	// Label is LabelStale, LabelCurrent or empty: the mechanical label, with
	// the tokens it rests on.
	Label      string  `json:"label,omitempty"`
	Checked    int     `json:"checked"`
	Missing    []Token `json:"missing,omitempty"`
	Unresolved []Token `json:"unresolved,omitempty"`
	// Model is the versioned Jev model that answered. Empty when no answer came
	// back.
	Model string `json:"model"`
	// Answers maps each question id to its answer. Null when the file was not
	// asked, and Reason says why.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Reason names why a file has no answers: one of the Reason constants.
	Reason string `json:"reason,omitempty"`
}

// answersFrom reads every question's answer out of one Ask result.
func answersFrom(got *jev.Answers) map[string]Answer {
	out := make(map[string]Answer, len(got.Noul)+len(got.Choice))
	for id, p := range got.Noul {
		out[id] = Answer{Noul: &p}
	}
	for id, c := range got.Choice {
		out[id] = Answer{Choice: c.Choice, Probabilities: c.Probabilities}
	}
	return out
}

// LogPath is the notes log, one JSON line per file asked about.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "notes.jsonl")
}

// lockRun takes the run lock beside the log. A held lock is ErrBusy.
func lockRun(logPath string) (func(), error) {
	release, err := battery.Lock(logPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}
