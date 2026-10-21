package stale

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Reasons a line carries instead of answers. They are fixed words, never an
// error message, so the log never holds a path or a bead's text.
const (
	ReasonBadResponse = "bad_response"
	ReasonAskFailed   = "ask_failed"
)

// ErrBusy means another pass holds the run lock.
var ErrBusy = errors.New("another stale pass holds the run lock")

// Answer is Jev's answer to one question. A noul sets Noul; the score sets
// Score and Confidence.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Score is the chosen level, 0 to len(levels)-1.
	Score *float64 `json:"score,omitempty"`
	// Confidence is Jev's confidence in a score, 0 to 1.
	Confidence *float64 `json:"confidence,omitempty"`
}

// Line is one line of the stale log: what Jev answered about one bead. It holds
// IDs, times, numbers, answers and a hash of the description, never a title or
// the description's text. A bead that could not be asked has null answers and a
// Reason.
type Line struct {
	// TS is when the line was logged. A bead is not asked again until AskAgain
	// has passed since this time.
	TS time.Time `json:"ts"`
	// Rig and Bead name the bead that was read.
	Rig  string `json:"rig"`
	Bead string `json:"bead"`
	// Type and Priority are the bead's issue type and its P.
	Type     string `json:"type"`
	Priority int    `json:"priority"`
	// UpdatedAt is when the bead was last written.
	UpdatedAt time.Time `json:"updated_at"`
	// DescHash is HashDescription of the bead's description when it was asked
	// about. The scorecard compares it with the description as it stands later,
	// to see a reshape. Empty on a line logged before the field existed.
	DescHash string `json:"desc_hash,omitempty"`
	// LandedOffered lists the landed beads the question offered, in the order
	// it gave them.
	LandedOffered []string `json:"landed_offered"`
	// Model is the versioned Jev model that answered. Empty when no answer
	// came back.
	Model string `json:"model"`
	// Answers maps each question id to its answer. Null when the bead was not
	// asked, and Reason says why.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Reason names why a bead has no answers: one of the Reason constants.
	Reason string `json:"reason,omitempty"`
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

// HashDescription is the hash a log line keeps of a bead's description: its
// SHA-256 in hex. The log holds the hash, never the text.
func HashDescription(desc string) string {
	sum := sha256.Sum256([]byte(desc))
	return hex.EncodeToString(sum[:])
}

// LogPath is the stale log, one JSON line per bead asked about.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "stale.jsonl")
}

// lockRun takes the run lock beside the log. A held lock is ErrBusy.
func lockRun(logPath string) (func(), error) {
	release, err := battery.Lock(logPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}
