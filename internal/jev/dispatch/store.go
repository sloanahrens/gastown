package dispatch

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// SizeAnswer is the answer to the size rubric: the level, 0 for the smallest,
// and Jev's confidence in it.
type SizeAnswer struct {
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
}

// Answers is what Jev said to the battery. Each Noul is the probability that
// its statement holds, 0 to 1.
type Answers struct {
	LandsFirstTry      float64    `json:"lands_first_try"`
	FitsTwentyMinutes  float64    `json:"fits_twenty_minutes"`
	AcceptanceTickable float64    `json:"acceptance_tickable"`
	NeedsStrongModel   float64    `json:"needs_strong_model"`
	TouchesDaemon      float64    `json:"touches_daemon"`
	Size               SizeAnswer `json:"size"`
}

// Line is one line of the dispatch log: what Jev predicted for one bead as it
// was placed on a seat. It holds bead IDs, the agent name, numbers and the
// model name only, never a title, a description, a prompt, an error message or
// key text.
type Line struct {
	// TS is when the answer was logged.
	TS time.Time `json:"ts"`
	// Bead is the bead the line is about.
	Bead string `json:"bead"`
	// AttachedAt is the bead's attached_at stamp, which marks the placement.
	// A bead placed twice earns a line for each.
	AttachedAt time.Time `json:"attached_at"`
	// Agent is the agent the bead was pinned or slung to, empty when unknown.
	Agent string `json:"agent"`
	// Answers is the battery's answers.
	Answers Answers `json:"answers"`
	// QuestionVersion is Version when the line was asked.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned model ID the API says answered, so a model
	// upgrade shows in the scorecard.
	Model string `json:"model"`
}

// LogPath is the dispatch log, one JSON line per bead placed.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "dispatch.jsonl")
}

// CursorPath is the file that remembers the last placement a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "dispatch.cursor.json")
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
var ErrBusy = errors.New("another jev dispatch pass is running")

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
