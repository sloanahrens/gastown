package logtriage

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Reasons a line carries instead of answers. They are fixed words, never an
// error message, so a reason never carries a path or the daemon's text.
const (
	ReasonBadResponse = "bad_response"
	ReasonAskFailed   = "ask_failed"
)

// ErrBusy means another pass holds the run lock: a second pass over the same
// cursor waits for the next interval rather than triaging the same lines.
var ErrBusy = errors.New("another jev logtriage pass is running")

// Answer is Jev's answer to one question about a group. A noul sets Noul, a
// score sets Score and Confidence, and a choice sets Choice and Probabilities.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Score is the index of the level Jev picked, into the question's levels.
	Score *float64 `json:"score,omitempty"`
	// Confidence is Jev's confidence in a score, 0 to 1.
	Confidence *float64 `json:"confidence,omitempty"`
	// Choice is the option Jev picked.
	Choice string `json:"choice,omitempty"`
	// Probabilities maps every option of a choice to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Line is one line of the triage log: what Jev answered about one group of
// warning and error lines, with the group's key, counts, times and samples.
type Line struct {
	// TS is when the answers were logged.
	TS time.Time `json:"ts"`
	// Key is the group's pattern key: the line with its ids, hashes, numbers,
	// durations and times replaced by placeholders.
	Key string `json:"key"`
	// Count is how many matching lines the group holds in this pass.
	Count int `json:"count"`
	// First and Last are the earliest and latest times the group's lines carry.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	// Samples are the first MaxSamples lines of the group, as the daemon wrote
	// them, each cut to maxSampleRunes.
	Samples []string `json:"samples"`
	// MatchedOn is the keyword that kept the group's first line, so the
	// filter can be audited against the lines it kept.
	MatchedOn string `json:"matched_on"`
	// Answers maps each question id to its answer. Null when the group was not
	// asked, and Reason says why.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned Jev model that answered. Empty when no answer
	// came back.
	Model string `json:"model"`
	// Reason names why a group has no answers: one of the Reason constants.
	Reason string `json:"reason,omitempty"`
}

// Cursor is how far into the daemon log a pass has read. Inode names the file
// it read, so a rotated log restarts at its first line; Offset is the byte
// after the last complete line the pass consumed. Pending holds the groups a
// pass's deadline ended before it asked about them, since their lines are
// consumed and cannot be read again.
type Cursor struct {
	Inode   uint64         `json:"inode"`
	Offset  int64          `json:"offset"`
	Pending []pendingGroup `json:"pending,omitempty"`
}

// pendingGroup is a group carried to the next pass: its count, time range,
// first samples and keyword so far.
type pendingGroup struct {
	Key       string    `json:"key"`
	Count     int       `json:"count"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Samples   []string  `json:"samples"`
	MatchedOn string    `json:"matched_on"`
}

// answersFrom reads every question's answer out of one Ask result.
func answersFrom(got *jev.Answers) map[string]Answer {
	out := make(map[string]Answer, len(got.Noul)+len(got.Score)+len(got.Choice))
	for id, p := range got.Noul {
		out[id] = Answer{Noul: &p}
	}
	for id, s := range got.Score {
		score, conf := s.Score, s.Confidence
		out[id] = Answer{Score: &score, Confidence: &conf}
	}
	for id, c := range got.Choice {
		out[id] = Answer{Choice: c.Choice, Probabilities: c.Probabilities}
	}
	return out
}

// LogPath is the triage log, one JSON line per group asked about.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "logtriage.jsonl")
}

// CursorPath is the file that remembers how far into the daemon log a pass read.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "logtriage.cursor.json")
}

// lockRun takes the pass's run lock without waiting, and returns the release.
func lockRun(cursorPath string) (func(), error) {
	release, err := battery.Lock(cursorPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}
