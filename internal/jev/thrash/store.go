package thrash

import (
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Answers is what Jev said to the battery. Each value is the probability that
// the question's statement holds, 0 to 1.
type Answers struct {
	WillRespawn float64 `json:"will_respawn"`
	NeedsSplit  float64 `json:"needs_split"`
}

// Line is one line of the thrash log: what Jev predicted for one bead as it was
// placed on a seat. It holds bead IDs, the agent name, numbers and the model
// name only, never a title, a description, a prompt, an error message or key
// text. The labels are not here: the scorecard adapter adds them afterwards.
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
	// Model is the versioned model ID the API says answered.
	Model string `json:"model"`
}

// LogPath is the thrash log, one JSON line per bead placed.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "thrash.jsonl")
}

// CursorPath is the file that remembers the last placement a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "thrash.cursor.json")
}
