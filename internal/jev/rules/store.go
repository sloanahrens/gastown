package rules

import (
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Answer is Jev's answer to one question. A noul sets Noul; a choice sets
// Choice and Probabilities, which map every option to its probability.
type Answer struct {
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Line is one line of the rules log: what Jev answered about one finished
// session. It holds ids, times, a hash of the excerpt and answers, never
// transcript text. A session that could not be asked has null answers and a
// Reason.
type Line struct {
	// TS is when the line was logged.
	TS time.Time `json:"ts"`
	// Rig, Bead and Polecat name the session that finished.
	Rig     string `json:"rig"`
	Bead    string `json:"bead"`
	Polecat string `json:"polecat"`
	// DoneAt is when gt done submitted the bead for landing.
	DoneAt time.Time `json:"done_at"`
	// ExcerptSHA256 is the hex SHA-256 of the excerpt Jev read. It ties a line
	// to the text without carrying any of it.
	ExcerptSHA256 string `json:"excerpt_sha256,omitempty"`
	// Model is the versioned Jev model that answered. Empty when no answer
	// came back.
	Model string `json:"model"`
	// Answers maps each question id to its answer. Null when the session was
	// not asked, and Reason says why.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Reason names why a session has no answers: one of the Reason constants.
	Reason string `json:"reason,omitempty"`
}

// Reasons a line carries instead of answers. They are fixed words, never an
// error message, so the log never holds a path or a transcript fragment.
const (
	ReasonNoTranscript         = "no_transcript"
	ReasonTranscriptUnreadable = "transcript_unreadable"
	// ReasonOtherSession means the seat's newest transcript began after the
	// submission, so it is a later session's, and this one was not read.
	ReasonOtherSession = "other_session"
	ReasonBadResponse  = "bad_response"
	// ReasonRequestRefused means Jev refused this session's request as
	// malformed or too large.
	ReasonRequestRefused = "request_refused"
)

// LogPath is the rules log, one JSON line per finished session.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "rules.jsonl")
}

// CursorPath is the file that remembers the last submission a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "rules.cursor.json")
}
