package dedupe

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Outcome says how a line's candidate came about. It is one of a fixed set of
// codes, never free text, so it tells a real "none" from a failure without the
// log holding an error message.
type Outcome string

const (
	// OutcomeAnswered: Jev answered with an offered option (or none) at or
	// above the confidence floor. Candidate is its choice.
	OutcomeAnswered Outcome = "answered"
	// OutcomeNoCandidates: the search found nothing to offer, so Jev was not
	// asked.
	OutcomeNoCandidates Outcome = "no_candidates"
	// OutcomeSearchFailed: the hm search failed, so Jev was not asked.
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
