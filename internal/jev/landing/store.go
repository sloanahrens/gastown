package landing

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Answer is Jev's answer to one question. A noul sets Noul; the score sets
// Score and Confidence.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Score is the chosen risk level, 0 to len(levels)-1.
	Score *float64 `json:"score,omitempty"`
	// Confidence is Jev's confidence in a score, 0 to 1.
	Confidence *float64 `json:"confidence,omitempty"`
}

// Line is one line of the landing log: what Jev answered about one landing.
// It holds bead IDs, path counts, numbers and answers only, never a prompt,
// a title, a diff or an error message.
type Line struct {
	// TS is when the answer was logged.
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
	// Answers maps each question id to its answer.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned Jev model that answered, so a model upgrade
	// shows in the scorecard.
	Model string `json:"model"`
}

// answersFrom reads every question's answer out of one Ask result.
func answersFrom(got *jev.Answers) map[string]Answer {
	out := make(map[string]Answer, len(got.Noul)+len(got.Score))
	for id, p := range got.Noul {
		out[id] = Answer{Noul: &p}
	}
	for id, s := range got.Score {
		score, conf := s.Score, s.Confidence
		out[id] = Answer{Score: &score, Confidence: &conf}
	}
	return out
}

// LogPath is the landing log, one JSON line per landing.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "landing.jsonl")
}

// CursorPath is the file that remembers the last landing a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "landing.cursor.json")
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
var ErrBusy = errors.New("another jev landing pass is running")

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
