package dedupe

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Outcome says how a line's candidate came about. It is one of a fixed set of
// codes, never free text, so a log's reader (the --recent table, an operator
// grep) tells a real "none" from a failure without the log holding an error
// message. A pass never branches on an outcome.
type Outcome string

const (
	// OutcomeAnswered: Jev answered with an offered option (or none) at or
	// above the confidence floor. Candidate is its choice.
	OutcomeAnswered Outcome = "answered"
	// OutcomeNoCandidates: the search found nothing to offer, so the Choice
	// was not asked. The Nouls and the Score are still asked.
	OutcomeNoCandidates Outcome = "no_candidates"
	// OutcomeReadFailed: at least one hit that could have been offered could
	// not be read (the bead store failed to show it, or its CreatedAt did not
	// parse) and no candidate was offered, so the Choice was not asked. Hits
	// filtered out alongside it do not change the label: the read failure is
	// what left the Choice unasked. The Nouls and the Score are still asked. A
	// line reading no_candidates would claim the search found nothing.
	OutcomeReadFailed Outcome = "read_failed"
	// OutcomeSearchFailed: the hm search failed, so no candidate was offered
	// and the Choice was not asked. The Nouls and the Score are still asked,
	// and a line with null answers says the call failed too.
	OutcomeSearchFailed Outcome = "search_failed"
	// OutcomeAskFailed: the Jev call failed (error, timeout, 429 after the
	// retry, or an unparseable reply).
	OutcomeAskFailed Outcome = "ask_failed"
	// OutcomeLowConfidence: Jev answered below the confidence floor.
	OutcomeLowConfidence Outcome = "low_confidence"
	// OutcomeUnknownOption: Jev named an option the question did not offer.
	OutcomeUnknownOption Outcome = "unknown_option"
)

// Line is one line of the suggestion log: what one finding was offered and
// what Jev answered. It holds bead IDs, numbers and an outcome code only,
// never a title, a description, a prompt, an error message or key text.
type Line struct {
	// TS is when the answer was logged.
	TS time.Time `json:"ts"`
	// Finding is the bead the line is about.
	Finding string `json:"finding"`
	// Candidate is the bead Jev chose as the duplicate, or "none".
	Candidate string `json:"candidate"`
	// Confidence is Jev's confidence in its answer, 0 when no answer came.
	Confidence float64 `json:"confidence"`
	// CandidatesOffered lists the beads the question offered, in the order
	// the question gave them.
	CandidatesOffered []string `json:"candidates_offered"`
	// Outcome tells an answered "none" from a failure that also logs none.
	Outcome Outcome `json:"outcome"`
	// Answers maps each question id asked to Jev's raw answer, with no floor
	// applied. It is null when the call failed.
	Answers map[string]Answer `json:"answers"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned Jev model that answered, empty when the call
	// failed.
	Model string `json:"model"`
}

// Answer is Jev's answer to one question. A noul sets Noul; a score sets Score
// and Confidence; a choice sets Choice and Confidence.
type Answer struct {
	// Choice is the option a choice question picked.
	Choice *string `json:"choice,omitempty"`
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Score is the chosen level, 0 to len(levels)-1.
	Score *float64 `json:"score,omitempty"`
	// Confidence is Jev's confidence in a choice or a score, 0 to 1.
	Confidence *float64 `json:"confidence,omitempty"`
}

// answersFrom reads every question's answer out of one Ask result.
func answersFrom(got *jev.Answers) map[string]Answer {
	out := make(map[string]Answer, len(got.Choice)+len(got.Noul)+len(got.Score))
	for id, c := range got.Choice {
		choice, conf := c.Choice, c.Confidence
		out[id] = Answer{Choice: &choice, Confidence: &conf}
	}
	for id, p := range got.Noul {
		out[id] = Answer{Noul: &p}
	}
	for id, s := range got.Score {
		score, conf := s.Score, s.Confidence
		out[id] = Answer{Score: &score, Confidence: &conf}
	}
	return out
}

// LogPath is the suggestion log, one JSON line per finding.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "dedupe.jsonl")
}

// CursorPath is the file that remembers the last finding a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "dedupe.cursor.json")
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
var ErrBusy = errors.New("another jev dedupe pass is running")

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

// waitFor pauses for d, or until ctx is done.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
