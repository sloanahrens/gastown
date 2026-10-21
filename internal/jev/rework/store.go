package rework

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Answer is Jev's answer to one question about one refused review. A noul sets
// Noul; a choice sets Choice and Probabilities.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Choice is the option Jev picked.
	Choice string `json:"choice,omitempty"`
	// Probabilities maps every option of the choice to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Line is one line of the rework log: what Jev answered about one om review
// that refused a landing. It holds ids, times, numbers and answers, never the
// findings the answers were judged from. Head is the submission the refusal
// judged, which the scorecard's label compares with what landed.
type Line struct {
	// TS is when the pass asked about the refusal.
	TS time.Time `json:"ts"`
	// Refused is when the landing logged the refused review. The scorecard's
	// label counts the bead's landings after it, so a landing between the
	// refusal and the pass is still the one that decides.
	Refused time.Time `json:"refused"`
	// Rig and Bead name the landing whose review refused the work.
	Rig  string `json:"rig"`
	Bead string `json:"bead"`
	// Attempt is the rework attempt the refusal belongs to, as the review log
	// counts it.
	Attempt int `json:"attempt"`
	// Head is the submission the refusal judged.
	Head string `json:"head"`
	// Verdict and Score are om's, as the review log recorded them.
	Verdict string  `json:"verdict"`
	Score   float64 `json:"score"`
	// Answers maps each question id to its answer.
	Answers map[string]Answer `json:"answers"`
	// Model is the versioned Jev model that answered.
	Model string `json:"model"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
}

// LogPath is the rework log, one JSON line per refused review.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "rework.jsonl")
}

// CursorPath is the file that remembers the last refusal a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "rework.cursor.json")
}

// ReadRecent returns the last n lines of the log, oldest first. A missing log
// is empty, not an error, and a line that does not parse is skipped.
func ReadRecent(path string, n int) ([]Line, error) {
	var lines []Line
	if err := battery.ReadRecent(path, n, &lines); err != nil {
		return nil, err
	}
	return lines, nil
}

// ErrBusy means another pass holds the run lock: a manual pass and the
// daemon's pass share one cursor, so they take turns.
var ErrBusy = errors.New("another jev rework pass is running")

// lockRun takes the pass's run lock without waiting, and returns the release.
// The lock sits beside the cursor, so every pass over one cursor contends on
// it, in this process and in others.
func lockRun(cursorPath string) (func(), error) {
	release, err := battery.Lock(cursorPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}
