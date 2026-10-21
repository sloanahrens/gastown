package attention

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Line is one line of the attention log: what one item was and what Jev
// answered. The values are the raw answers as Jev returned them, with no floor
// and no rewrite, so a reader can judge the battery from the log alone.
type Line struct {
	// TS is when the answer was logged.
	TS time.Time `json:"ts"`
	// Key is the attention item the line is about.
	Key string `json:"key"`
	// Kind is the item's kind.
	Kind attention.Kind `json:"kind"`
	// Severity is the item's severity.
	Severity attention.Severity `json:"severity"`
	// Rig, Bead and SHA are the item's, empty when it had none.
	Rig  string `json:"rig"`
	Bead string `json:"bead"`
	SHA  string `json:"sha"`
	// FirstSeen is when the item was raised: the time of the "new" transition
	// the line asks about, so each raise of a key has its own line.
	FirstSeen time.Time `json:"first_seen"`
	// Answers is what Jev answered, or null when the call failed.
	Answers *Answers `json:"answers"`
	// QuestionVersion is the version of the question set asked.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned model ID that answered, empty when the call
	// failed.
	Model string `json:"model"`
	// Live is whether the item was still open in state.json when the pass
	// asked. When it was not, Rig, Bead and SHA are empty and the question
	// has no other open items beside it.
	Live bool `json:"live"`
}

// Answers holds one item's answers to the question set.
type Answers struct {
	// NeedsHuman is the probability that a person has to look at or decide
	// the item.
	NeedsHuman float64 `json:"needs_human"`
	// ClearsOnItsOwn is the probability that the condition goes away without
	// anyone acting.
	ClearsOnItsOwn float64 `json:"clears_on_its_own"`
	// RoutineActionFixes is the probability that a known routine action
	// settles the item.
	RoutineActionFixes float64 `json:"routine_action_fixes"`
	// RepeatsOpenItem is the probability that another open item describes the
	// same condition.
	RepeatsOpenItem float64 `json:"repeats_open_item"`
	// Urgency is how soon the item should be seen.
	Urgency Urgency `json:"urgency"`
}

// Urgency is the score answer: the chosen level and Jev's confidence in it.
type Urgency struct {
	// Score indexes the urgency levels, low to high.
	Score float64 `json:"score"`
	// Confidence is Jev's confidence in Score, 0 to 1.
	Confidence float64 `json:"confidence"`
}

// LogPath is the attention log, one JSON line per item.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "attention.jsonl")
}

// CursorPath is the file that remembers the last item a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "attention.cursor.json")
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
