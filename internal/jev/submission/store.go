package submission

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Answer is Jev's answer to one question. A noul sets Noul; the score sets
// Score and Confidence; a choice sets Choice, Probabilities and Positive.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Score is the chosen risk level, 0 to len(levels)-1.
	Score *float64 `json:"score,omitempty"`
	// Confidence is Jev's confidence in a score, 0 to 1.
	Confidence *float64 `json:"confidence,omitempty"`
	// Choice is the option Jev picked.
	Choice string `json:"choice,omitempty"`
	// Probabilities maps every option of a choice to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Positive names the choice option that counts as a yes.
	Positive string `json:"positive,omitempty"`
}

// Line is one line of the submission log: what Jev answered about one
// submission. It holds ids, times, numbers and answers only, never a prompt,
// a title, a diff or an error message.
type Line struct {
	// TS is when the answer was logged.
	TS time.Time `json:"ts"`
	// Bead is the work bead submitted for landing.
	Bead string `json:"bead"`
	// Branch is the branch the bead asks to land.
	Branch string `json:"branch"`
	// Head is the commit the bead submitted.
	Head string `json:"head"`
	// Target is the branch head lands on.
	Target string `json:"target"`
	// Submitted is the submission time the pass ordered the bead by.
	Submitted time.Time `json:"submitted"`
	// Agent is the agent the bead names: its agent: label, else the agent
	// entry in attached_vars. Empty for a submission that names none.
	Agent string `json:"agent"`
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
	out := make(map[string]Answer, len(got.Noul)+len(got.Score)+len(got.Choice))
	for id, p := range got.Noul {
		out[id] = Answer{Noul: &p}
	}
	for id, s := range got.Score {
		score, conf := s.Score, s.Confidence
		out[id] = Answer{Score: &score, Confidence: &conf}
	}
	for id, c := range got.Choice {
		out[id] = Answer{Choice: c.Choice, Probabilities: c.Probabilities, Positive: choicePositive[id]}
	}
	return out
}

// LogPath is the submission log, one JSON line per submission.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "submission.jsonl")
}

// CursorPath is the file that remembers the last submission a pass asked
// about.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "submission.cursor.json")
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
var ErrBusy = errors.New("another jev submission pass is running")

// lockRun takes the pass's run lock without waiting, and returns the release.
// The lock sits beside the cursor, so every pass over one cursor contends on
// it, in this process and in others.
func lockRun(cursorPath string) (func(), error) {
	release, err := battery.Lock(lockPath(cursorPath))
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}

// lockPath is the lock file beside base. Every lock derived from a path is
// derived here, so a pass and a fetch guarding the same thing cannot drift onto
// different files.
func lockPath(base string) string { return base + ".lock" }

// FetchLockPath is the lock every fetch into the shared clone holds. It sits
// beside the clone, off the pass's run lock, so a fetch never waits a whole
// pass out.
func FetchLockPath(townRoot string) string {
	return lockPath(ClonePath(townRoot))
}

// lockPoll is how long a waiter sleeps between attempts at a lock.
const lockPoll = 100 * time.Millisecond

// WaitFetchLock takes the shared clone's fetch lock, waiting for a fetch in
// flight to finish, and returns the release. A battery that only reads the
// clone takes it around its fetch: two fetches into one clone fail each other,
// and the pass's run lock is held for a whole pass, so taking that lock would
// stall the fetch and let it starve the pass.
func WaitFetchLock(ctx context.Context, townRoot string) (func(), error) {
	path := FetchLockPath(townRoot)
	for {
		release, err := battery.Lock(path)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, battery.ErrBusy) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// Cloner is the part of the shared clone another battery reads through.
// *Clone satisfies it.
type Cloner interface {
	Sync(ctx context.Context) error
	Run(ctx context.Context, args ...string) (string, error)
}

// FetchLocked fetches the shared clone under the clone's fetch lock. Every
// battery that fetches into the clone wraps it in this: the submission pass
// fetches under that lock, so a fetch that skipped it would fail one of the two
// and lose its work. Run reads the clone and takes no lock. The pass's run lock
// is not used: it is held for a whole pass, and the fetch has no business
// waiting that out (gt-ckobj).
type FetchLocked struct {
	Cloner
	TownRoot string
}

// Sync waits for the clone's fetch lock, then fetches.
func (f FetchLocked) Sync(ctx context.Context) error {
	return syncUnderFetchLock(ctx, f.TownRoot, f.Cloner.Sync)
}

// syncUnderFetchLock waits for the fetch lock at townRoot, then runs sync with
// it held.
func syncUnderFetchLock(ctx context.Context, townRoot string, sync func(context.Context) error) error {
	release, err := WaitFetchLock(ctx, townRoot)
	if err != nil {
		return err
	}
	defer release()
	return sync(ctx)
}
