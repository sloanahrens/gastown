package targets

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

// Line is one line of the targets log: what Jev answered about one slice file.
// It holds ids, the rank's numbers and answers only, never the state, the file
// body or an error message. Lines are appended in rank order, so a line's
// position is the file's place in the slice.
type Line struct {
	// TS is when the answer was logged.
	TS time.Time `json:"ts"`
	// Run is the deep-review run bead whose slice this file was in.
	Run string `json:"run"`
	// Path is the file's repository path.
	Path string `json:"path"`
	// Tier is the rank row's tier: 1 for a file reviewed in the cooldown.
	Tier int `json:"tier"`
	// RiskScore is the rank's score for the file.
	RiskScore float64 `json:"risk_score"`
	// Bytes is the file's size in bytes, as the rank measured it.
	Bytes int `json:"bytes"`
	// Model is the versioned Jev model that answered, so a model upgrade shows
	// in the scorecard.
	Model string `json:"model"`
	// Answers maps each question id to its answer.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
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

// LogPath is the targets log, one JSON line per scored file.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "targets.jsonl")
}
