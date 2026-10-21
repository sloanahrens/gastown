package drift

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Answer is Jev's answer to one question about a doc section. A noul sets
// Noul; a choice sets Choice and Probabilities.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Choice is the option Jev picked.
	Choice string `json:"choice,omitempty"`
	// Probabilities maps every option of a choice to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// SectionAnswers is what Jev answered about one doc section a landing named.
type SectionAnswers struct {
	// Doc is the doc file's path in the rig's repository.
	Doc string `json:"doc"`
	// Heading is the section's heading line, without its hashes.
	Heading string `json:"heading"`
	// Answers maps each question id to its answer.
	Answers map[string]Answer `json:"answers"`
}

// Line is one line of the drift log: what Jev answered about one landing and
// the doc sections it was asked about. It holds ids, paths, headings, numbers
// and answers only, never a diff, a doc's text or an error message.
type Line struct {
	// TS is when the answers were logged.
	TS time.Time `json:"ts"`
	// Bead is the landed bead.
	Bead string `json:"bead"`
	// Rig is the rig the bead landed in.
	Rig string `json:"rig"`
	// Head is the branch head that landed.
	Head string `json:"head"`
	// LandedAt is when the landing worker landed it.
	LandedAt time.Time `json:"landed_at"`
	// Agent is the seat's agent from the landing record; empty for a crew
	// landing.
	Agent string `json:"agent,omitempty"`
	// Sections holds one entry per doc section that got an answer. It is empty
	// when no doc section names anything the landing's diff touches.
	Sections []SectionAnswers `json:"sections"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned Jev model that answered.
	Model string `json:"model"`
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

// LogPath is the drift log, one JSON line per landing.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "drift.jsonl")
}

// CursorPath is the file that remembers the last landing a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "drift.cursor.json")
}

// ErrBusy means another pass holds the run lock: a second pass over the same
// cursor waits for the next interval rather than asking the same landings.
var ErrBusy = errors.New("another jev drift pass is running")

// lockRun takes the pass's run lock without waiting, and returns the release.
func lockRun(cursorPath string) (func(), error) {
	release, err := battery.Lock(cursorPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}
