package progress

import (
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
)

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

// Line is one line of the progress log: what Jev answered about one nudge. It
// holds ids, times, numbers and answers only, never a title, a transcript
// fragment or an error message. A nudge that could not be asked has null
// answers and a Reason.
type Line struct {
	// TS is when the line was logged.
	TS time.Time `json:"ts"`
	// Rig, Polecat, Bead and Session name the seat that was nudged.
	Rig     string `json:"rig"`
	Polecat string `json:"polecat"`
	Bead    string `json:"bead"`
	Session string `json:"session"`
	// NudgeAt is when the guard delivered the nudge.
	NudgeAt time.Time `json:"nudge_at"`
	// Model is the versioned Jev model that answered. Empty when no answer
	// came back.
	Model string `json:"model"`
	// Answers maps each question id to its answer. Null when the nudge was
	// not asked, and Reason says why.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Reason names why a nudge has no answers: one of the Reason constants.
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

// LogPath is the progress log, one JSON line per nudge.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "progress.jsonl")
}
